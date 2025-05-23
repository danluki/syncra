package taskvault

import (
	"fmt"
	"net"
	"sync"
	"time"

	metrics "github.com/hashicorp/go-metrics"
	"github.com/hashicorp/raft"
	"github.com/hashicorp/serf/serf"
	"go.uber.org/zap"
)

const (
	barrierWriteTimeout = 2 * time.Minute
)

func (a *Agent) monitorLeadership() {
	var weAreLeaderCh chan struct{}
	var leaderLoop sync.WaitGroup
	for {
		a.logger.Info("taskvault: monitoring leadership")
		select {
		case isLeader := <-a.leaderCh:
			switch {
			case isLeader:
				if weAreLeaderCh != nil {
					a.logger.Error("taskvault: attempted to start the leader loop while running")
					continue
				}

				weAreLeaderCh = make(chan struct{})
				leaderLoop.Add(1)
				go func(ch chan struct{}) {
					defer leaderLoop.Done()
					a.leaderLoop(ch)
				}(weAreLeaderCh)
				a.logger.Info("taskvault: cluster leadership acquired")

			default:
				if weAreLeaderCh == nil {
					a.logger.Error("taskvault: attempted to stop the leader loop while not running")
					continue
				}

				a.logger.Debug("taskvault: shutting down leader loop")
				close(weAreLeaderCh)
				leaderLoop.Wait()
				weAreLeaderCh = nil
				a.logger.Info("taskvault: cluster leadership lost")
			}

		case <-a.shutdowner:
			return
		}
	}
}

func (a *Agent) leaderLoop(stopCh chan struct{}) {
	var refreshCh chan serf.Member

REFRESH:
	refreshCh = nil
	interval := time.After(a.config.RefreshInterval)

	start := time.Now()
	barrier := a.raft.Barrier(barrierWriteTimeout)
	if err := barrier.Error(); err != nil {
		a.logger.Error("taskvault: failed to wait for barrier", zap.Error(err))
		goto WAIT
	}
	metrics.MeasureSince([]string{"taskvault", "leader", "barrier"}, start)

	if err := a.Refresh(); err != nil {
		a.logger.Error("failed to ", zap.Error(err))
		goto WAIT
	}

	refreshCh = a.refreshCh

	select {
	case <-stopCh:
		return
	default:
	}

WAIT:
	for {
		select {
		case <-stopCh:
			return
		case <-a.shutdowner:
			return
		case <-interval:
			goto REFRESH
		case member := <-refreshCh:
			if err := a.RefreshMember(member); err != nil {
				a.logger.Error("taskvault: failed to Refresh member", zap.Error(err))
			}
		}
	}
}

func (a *Agent) Refresh() error {
	defer metrics.MeasureSince(
		[]string{"taskvault", "leader", "Refresh"}, time.Now(),
	)

	members := a.serf.Members()
	for _, member := range members {
		if err := a.RefreshMember(member); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) RefreshMember(member serf.Member) error {
	parts := toServerPart(member)
	if parts == nil {
		return nil
	}
	defer metrics.MeasureSince(
		[]string{
			"taskvault", "leader", "RefreshMember",
		}, time.Now(),
	)

	var err error
	switch member.Status {
	case serf.StatusAlive:
		err = a.addRaftPeer(member, parts)
	case serf.StatusLeft:
		err = a.removeRaftPeer(member, parts)
	}
	if err != nil {
		a.logger.Error("failed to Refresh member", zap.Error(err), zap.Any("member", member))
		return err
	}
	return nil
}

func (a *Agent) addRaftPeer(m serf.Member, parts *ServerParts) error {
	members := a.serf.Members()
	if parts.Bootstrap {
		for _, member := range members {
			parts := toServerPart(member)
			if parts == nil {
				continue
			}

			if member.Name != m.Name && parts.Bootstrap {
				a.logger.Errorf(
					"taskvault: '%v' and '%v' are both in bootstrap mode..",
					m.Name,
					member.Name,
				)
				return nil
			}
		}
	}

	addr := (&net.TCPAddr{IP: m.Addr, Port: parts.Port}).String()
	configFuture := a.raft.GetConfiguration()
	if err := configFuture.Error(); err != nil {
		a.logger.Error("taskvault: failed to get raft configuration", zap.Error(err))
		return err
	}

	if m.Name == a.config.NodeName {
		if l := len(configFuture.Configuration().Servers); l < 3 {
			a.logger.Debug(
				"taskvault: Skipping self join check",
				zap.String("peer", m.Name),
			)
			return nil
		}
	}

	for _, server := range configFuture.Configuration().Servers {
		if server.Address == raft.ServerAddress(addr) || server.ID == raft.ServerID(parts.ID) {
			if server.Address == raft.ServerAddress(addr) && server.ID == raft.ServerID(parts.ID) {
				return nil
			}
			if server.Address == raft.ServerAddress(addr) {
				future := a.raft.RemoveServer(server.ID, 0, 0)
				if err := future.Error(); err != nil {
					return fmt.Errorf(
						"error removing server %q: %s",
						server.Address,
						err,
					)
				}
			}
		}
	}

	addFuture := a.raft.AddVoter(
		raft.ServerID(parts.ID), raft.ServerAddress(addr), 0, 0,
	)
	if err := addFuture.Error(); err != nil {
		return err
	}

	return nil
}

func (a *Agent) removeRaftPeer(m serf.Member, parts *ServerParts) error {
	if m.Name == a.config.NodeName {
		a.logger.Warn(
			"removing self should be done by follower", "name",
			a.config.NodeName,
		)
		return nil
	}

	configFuture := a.raft.GetConfiguration()
	if err := configFuture.Error(); err != nil {
		return err
	}

	for _, server := range configFuture.Configuration().Servers {
		if server.ID == raft.ServerID(parts.ID) {
			future := a.raft.RemoveServer(raft.ServerID(parts.ID), 0, 0)
			if err := future.Error(); err != nil {
				return err
			}
			break
		}
	}

	return nil
}
