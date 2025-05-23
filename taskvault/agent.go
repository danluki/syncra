package taskvault

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/danluki/taskvault/pkg/types"
	"github.com/hashicorp/memberlist"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb"
	"github.com/hashicorp/serf/serf"
	"github.com/soheilhy/cmux"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	raftTimeout      = 30 * time.Second
	raftLogCacheSize = 512
	raftMultiplier   = 1
)

var (
	ErrLeaderNotFound   = errors.New("no member leader found")
	ErrNoSuitableServer = errors.New("no suitable server found")
)

type Node = serf.Member

type Agent struct {
	Store  SyncraStorage
	config *Config

	serfEventer chan serf.Event
	shutdowner  chan struct{}

	raftTransport *raft.NetworkTransport
	raft          *raft.Raft
	serf          *serf.Serf
	HTTPTransport Transport
	raftStore     RaftStore
	GRPCClient    TaskvaultGRPCClient
	raftLayer     *RaftLayer
	refreshCh     chan serf.Member
	GRPCServer    TaskvaultGRPCServer
	retryJoinCh   chan error
	leaderCh      <-chan bool
	serverLookup  *ServerLookup
	listener      net.Listener

	logger *zap.SugaredLogger

	raftInmemStore *raft.InmemStore
}

func NewAgent(config *Config) *Agent {
	agent := &Agent{
		config:       config,
		retryJoinCh:  make(chan error),
		serverLookup: NewServerLookup(),
	}

	return agent
}

func (a *Agent) Start() error {
	a.logger = InitLogger(a.config.LogLevel, a.config.NodeName)

	var err error
	if err = a.config.normalizeAddrs(); err != nil {
		if !errors.Is(err, ErrResolvingHost) {
			return err
		}
	}

	a.serf, err = a.setupSerf()
	if err != nil {
		return fmt.Errorf("agent: Can not setup serf, %s", err)
	}

	if len(a.config.RetryJoin) == 0 {
		_, err := a.join(a.config.StartJoin, true)
		if err != nil {
			a.logger.With(
				zap.Error(err),
				zap.Any("servers", a.config.StartJoin),
			).Warn("agent: Can not join")
		}
	} else {
		a.retryJoinLAN()
	}

	if a.config.AdvertiseRPCPort == 0 {
		a.config.AdvertiseRPCPort = a.config.RPCPort
	}

	addr := a.bindRPCAddr()
	a.listener, err = net.Listen("tcp", addr)
	if err != nil {
		panic(err)
	}

	a.StartServer()

	if a.GRPCClient == nil {
		a.GRPCClient = NewGRPCClient(nil, a, a.logger)
	}

	tags := a.serf.LocalMember().Tags
	tags["rpc_addr"] = a.advertiseRPCAddr()
	tags["port"] = strconv.Itoa(a.config.AdvertiseRPCPort)
	if err := a.serf.SetTags(tags); err != nil {
		return fmt.Errorf("agent: Error setting tags: %w", err)
	}

	go a.eventLoop()

	return nil
}

func (a *Agent) RetryJoinCh() <-chan error {
	return a.retryJoinCh
}

func (a *Agent) JoinLAN(addrs []string) (int, error) {
	return a.serf.Join(addrs, true)
}

func (a *Agent) Stop() error {
	a.logger.Info("agent: Called member stop, now stopping")

	_ = a.raft.Shutdown()

	if err := a.Store.Shutdown(); err != nil {
		return err
	}

	if err := a.serf.Leave(); err != nil {
		return err
	}

	if err := a.serf.Shutdown(); err != nil {
		return err
	}

	return nil
}

func (a *Agent) setupRaft() error {
	if a.config.BootstrapExpect == 1 {
		a.config.Bootstrap = true
	}

	logger := io.Discard
	if a.logger.Level() == zapcore.DebugLevel {
		logger = os.Stdout
	}

	transportConfig := &raft.NetworkTransportConfig{
		Stream:                a.raftLayer,
		MaxPool:               3,
		Timeout:               raftTimeout,
		ServerAddressProvider: a.serverLookup,
	}
	transport := raft.NewNetworkTransportWithConfig(transportConfig)
	a.raftTransport = transport

	config := raft.DefaultConfig()

	raftMultiplier := raftMultiplier
	config.HeartbeatTimeout = config.HeartbeatTimeout * time.Duration(raftMultiplier)
	config.ElectionTimeout = config.ElectionTimeout * time.Duration(raftMultiplier)
	config.LeaderLeaseTimeout = config.LeaderLeaseTimeout * time.Duration(raftMultiplier)

	config.LogOutput = logger
	config.LocalID = raft.ServerID(a.config.NodeName)

	var logStore raft.LogStore
	var stableStore raft.StableStore
	var snapshots raft.SnapshotStore
	if a.config.DevMode {
		store := raft.NewInmemStore()
		stableStore = store
		logStore = store
		snapshots = raft.NewDiscardSnapshotStore()
		a.raftInmemStore = store
	} else {
		var err error

		snapshots, err = raft.NewFileSnapshotStore(
			filepath.Join(
				a.config.DataDir, "raft",
			), 3, logger,
		)
		if err != nil {
			return fmt.Errorf("file snapshot store: %s", err)
		}

		if a.raftStore == nil {
			a.raftStore, err = raftboltdb.NewBoltStore(
				filepath.Join(
					a.config.DataDir, "raft", "raft.db",
				),
			)
			if err != nil {
				return fmt.Errorf("error creating new raft store: %s", err)
			}
		}
		stableStore = a.raftStore

		cacheStore, err := raft.NewLogCache(raftLogCacheSize, a.raftStore)
		if err != nil {
			a.raftStore.Close()
			return err
		}
		logStore = cacheStore
	}

	if a.config.Bootstrap || a.config.DevMode {
		hasState, err := raft.HasExistingState(logStore, stableStore, snapshots)
		if err != nil {
			return err
		}
		if !hasState {
			configuration := raft.Configuration{
				Servers: []raft.Server{
					{
						ID:      config.LocalID,
						Address: transport.LocalAddr(),
					},
				},
			}
			if err := raft.BootstrapCluster(
				config, logStore, stableStore, snapshots, transport,
				configuration,
			); err != nil {
				return err
			}
		}
	}

	fsm := newFSM(a.Store, a.logger)
	rft, err := raft.NewRaft(
		config, fsm, logStore, stableStore, snapshots, transport,
	)
	if err != nil {
		return fmt.Errorf("new raft: %s", err)
	}
	a.leaderCh = rft.LeaderCh()
	a.raft = rft

	return nil
}

func (a *Agent) setupSerf() (*serf.Serf, error) {
	bindIP, bindPort, err := a.config.AddrParts(a.config.BindAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid bind address: %s", err)
	}

	var advertiseIP string
	var advertisePort int
	if a.config.AdvertiseAddr != "" {
		advertiseIP, advertisePort, err = a.config.AddrParts(a.config.AdvertiseAddr)
		if err != nil {
			return nil, fmt.Errorf("invalid advertise address: %s", err)
		}
	}

	encryptKey, err := base64.StdEncoding.DecodeString(a.config.EncryptKey)
	if err != nil {
		return nil, fmt.Errorf("invalid encryption key: %s", err)
	}

	serfConfig := serf.DefaultConfig()
	serfConfig.Init()

	serfConfig.Tags["version"] = Version
	if a.config.Bootstrap {
		serfConfig.Tags["bootstrap"] = "1"
	}
	if a.config.BootstrapExpect != 0 {
		serfConfig.Tags["expect"] = fmt.Sprintf("%d", a.config.BootstrapExpect)
	}

	switch a.config.Profile {
	case "lan":
		serfConfig.MemberlistConfig = memberlist.DefaultLANConfig()
	case "wan":
		serfConfig.MemberlistConfig = memberlist.DefaultWANConfig()
	case "local":
		serfConfig.MemberlistConfig = memberlist.DefaultLocalConfig()
	default:
		return nil, fmt.Errorf("unknown profile: %s", a.config.Profile)
	}

	serfConfig.MemberlistConfig.BindAddr = bindIP
	serfConfig.MemberlistConfig.BindPort = bindPort
	serfConfig.MemberlistConfig.AdvertiseAddr = advertiseIP
	serfConfig.MemberlistConfig.AdvertisePort = advertisePort
	serfConfig.MemberlistConfig.SecretKey = encryptKey
	serfConfig.NodeName = a.config.NodeName
	serfConfig.CoalescePeriod = 3 * time.Second
	serfConfig.QuiescentPeriod = time.Second
	serfConfig.UserCoalescePeriod = 3 * time.Second
	serfConfig.UserQuiescentPeriod = time.Second
	serfConfig.ReconnectTimeout, err = time.ParseDuration(a.config.SerfReconnectTimeout)

	if err != nil {
		a.logger.Fatal(err)
	}

	a.serfEventer = make(chan serf.Event, 4096)
	serfConfig.EventCh = a.serfEventer

	a.logger.Info("agent: taskvault agent starting")

	if a.logger.Level() == zapcore.DebugLevel {
		serfConfig.LogOutput = os.Stdout
		serfConfig.MemberlistConfig.LogOutput = os.Stdout
	} else {
		serfConfig.LogOutput = io.Discard
		serfConfig.MemberlistConfig.LogOutput = io.Discard
	}

	serf, err := serf.Create(serfConfig)
	if err != nil {
		a.logger.Error(err)
		return nil, err
	}
	return serf, nil
}

func (a *Agent) StartServer() {
	var err error
	if a.Store == nil {
		a.Store, err = NewStore(a.logger)
		if err != nil {
			panic(err)
		}
	}

	a.HTTPTransport = NewTransport(a, a.logger)
	a.HTTPTransport.ServeHTTP()

	tcpm := cmux.New(a.listener)
	var grpcl, raftl net.Listener

	a.raftLayer = NewRaftLayer(a.logger)

	grpcl = tcpm.MatchWithWriters(
		cmux.HTTP2MatchHeaderFieldSendSettings(
			"content-type", "application/grpc",
		),
	)

	raftl = tcpm.Match(cmux.Any())

	a.GRPCServer = NewGRPCServer(a, a.logger)
	if err := a.GRPCServer.Serve(grpcl); err != nil {
		a.logger.With(zap.Error(err)).Fatal("agent: RPC server failed to start")
	}

	a.raftLayer.Open(raftl)

	if err := a.setupRaft(); err != nil {
		a.logger.With(zap.Error(err)).Fatal("agent: Raft layer failed to start")
	}

	go func() {
		if err := tcpm.Serve(); err != nil {
			a.logger.Fatal(err)
		}
	}()

	go a.monitorLeadership()
}

func (a *Agent) leaderMember() (*serf.Member, error) {
	l := a.raft.Leader()
	for _, member := range a.serf.Members() {
		if member.Tags["rpc_addr"] == string(l) {
			return &member, nil
		}
	}
	return nil, ErrLeaderNotFound
}

func (a *Agent) IsLeader() bool {
	return a.raft.State() == raft.Leader
}

func (a *Agent) Servers() (members []*ServerParts) {
	for _, member := range a.serf.Members() {
		parts := toServerPart(member)
		if parts == nil || member.Status != serf.StatusAlive {
			continue
		}
		members = append(members, parts)
	}
	return members
}

func (a *Agent) eventLoop() {
	internalShutdowner := a.serf.ShutdownCh()
	a.logger.Info("agent: Listen for events")
	for {
		select {
		case e := <-a.serfEventer:
			a.logger.With(zap.String("event", e.String())).Info("agent: Received event")

			if me, ok := e.(serf.MemberEvent); ok {
				for _, member := range me.Members {
					a.logger.With(
						zap.String("node", a.config.NodeName),
						zap.String("member", member.Name),
						zap.Any("event", e.EventType()),
					).Debug("agent: Member event")
				}

				switch e.EventType() {
				case serf.EventMemberJoin:
					a.nodeJoin(me, true)
					a.reapEvent(me)
				case serf.EventMemberLeave, serf.EventMemberFailed:
					a.nodeFailed(me)
					a.reapEvent(me)
				case serf.EventMemberReap:
					a.reapEvent(me)
				case serf.EventMemberUpdate:
					a.nodeJoin(me, false)
					a.reapEvent(me)
				default:
					a.logger.Warn("agent: Unhandled serf event", zap.String("event", e.String()))
				}
			}
		case <-internalShutdowner:
			a.logger.Warn("agent: Serf shutdown detected, quitting")
			return
		}
	}
}

func (a *Agent) join(addrs []string, replay bool) (n int, err error) {
	a.logger.Infof("agent: joining: %v replay: %v", addrs, replay)
	n, err = a.serf.Join(addrs, !replay)
	if n > 0 {
		a.logger.Infof("agent: joined: %d nodes", n)
	}
	if err != nil {
		a.logger.Warnf("agent: error joining: %v", err)
	}

	return
}

func (a *Agent) advertiseRPCAddr() string {
	bindIP := a.serf.LocalMember().Addr
	return net.JoinHostPort(
		bindIP.String(), strconv.Itoa(a.config.AdvertiseRPCPort),
	)
}

func (a *Agent) bindRPCAddr() string {
	bindIP, _, _ := a.config.AddrParts(a.config.BindAddr)
	return net.JoinHostPort(bindIP, strconv.Itoa(a.config.RPCPort))
}

func (a *Agent) applySetPair(pair *types.Pair) error {
	cmd, err := Encode(AddPairType, pair)
	if err != nil {
		return err
	}

	af := a.raft.Apply(cmd, raftTimeout)
	if err := af.Error(); err != nil {
		return err
	}

	return nil
}
