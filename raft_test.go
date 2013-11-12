package raft

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"os"
	"testing"
	"time"
)

// MockFSM is an implementation of the FSM interface, and just stores
// the logs sequentially
type MockFSM struct {
	logs [][]byte
}

func (m *MockFSM) Apply(log []byte) {
	m.logs = append(m.logs, log)
}

// Return configurations optimized for in-memory
func inmemConfig() *Config {
	return &Config{
		HeartbeatTimeout: 5 * time.Millisecond,
		ElectionTimeout:  10 * time.Millisecond,
		CommitTimeout:    time.Millisecond,
		MaxAppendEntries: 16,
	}
}

func TestRaft_StartStop(t *testing.T) {
	dir, store := LevelDBTestStore(t)
	defer os.RemoveAll(dir)
	defer store.Close()

	_, trans := NewInmemTransport()
	fsm := &MockFSM{}
	conf := DefaultConfig()
	peers := &StaticPeers{}

	raft, err := NewRaft(conf, fsm, store, store, peers, trans)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer raft.Shutdown()
}

func TestRaft_SingleNode(t *testing.T) {
	dir, store := LevelDBTestStore(t)
	defer os.RemoveAll(dir)
	defer store.Close()

	_, trans := NewInmemTransport()
	fsm := &MockFSM{}
	conf := inmemConfig()
	peers := &StaticPeers{}

	raft, err := NewRaft(conf, fsm, store, store, peers, trans)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer raft.Shutdown()

	time.Sleep(15 * time.Millisecond)

	// Should be leader
	if s := raft.State(); s != Leader {
		t.Fatalf("expected leader: %v", s)
	}

	// Should be able to apply
	future := raft.Apply([]byte("test"), time.Millisecond)
	if err := future.Error(); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Check that it is applied to the FSM
	if len(fsm.logs) != 1 {
		t.Fatalf("did not apply to FSM!")
	}
}

type cluster struct {
	dirs   []string
	stores []*LevelDBStore
	fsms   []*MockFSM
	trans  []*InmemTransport
	rafts  []*Raft
}

func (c *cluster) Close() {
	for _, r := range c.rafts {
		r.Shutdown()
	}
	for _, s := range c.stores {
		s.Close()
	}
	for _, d := range c.dirs {
		os.RemoveAll(d)
	}
}

func (c *cluster) GetInState(s RaftState) []*Raft {
	in := make([]*Raft, 0, 1)
	for _, r := range c.rafts {
		if r.State() == s {
			in = append(in, r)
		}
	}
	return in
}

func (c *cluster) Leader() *Raft {
	timeout := time.AfterFunc(250*time.Millisecond, func() {
		panic("timeout waiting for leader")
	})
	defer timeout.Stop()

	for len(c.GetInState(Leader)) < 1 {
		time.Sleep(time.Millisecond)
	}
	leaders := c.GetInState(Leader)
	if len(leaders) != 1 {
		panic(fmt.Errorf("expected one leader: %v", leaders))
	}
	return leaders[0]
}

func (c *cluster) FullyConnect() {
	for i, t1 := range c.trans {
		for j, t2 := range c.trans {
			if i != j {
				t1.Connect(t2.LocalAddr(), t2)
				t2.Connect(t1.LocalAddr(), t1)
			}
		}
	}
}

func (c *cluster) Disconnect(a net.Addr) {
	for _, t := range c.trans {
		if t.localAddr == a {
			t.DisconnectAll()
		} else {
			t.Disconnect(a)
		}
	}
}

func (c *cluster) EnsureSame(t *testing.T) {
	first := c.fsms[0]
	for i, fsm := range c.fsms {
		if i == 0 {
			continue
		}

		if len(first.logs) != len(fsm.logs) {
			t.Fatalf("length mismatch: %d %d",
				len(first.logs), len(fsm.logs))
		}

		for idx := 0; idx < len(first.logs); idx++ {
			if bytes.Compare(first.logs[idx], fsm.logs[idx]) != 0 {
				t.Fatalf("log mismatch at index %d", idx)
			}
		}
	}
}

func MakeCluster(n int, t *testing.T, conf *Config) *cluster {
	c := &cluster{}
	peers := make([]net.Addr, 0, n)

	// Setup the stores and transports
	for i := 0; i < n; i++ {
		dir, store := LevelDBTestStore(t)
		c.dirs = append(c.dirs, dir)
		c.stores = append(c.stores, store)
		c.fsms = append(c.fsms, &MockFSM{})

		addr, trans := NewInmemTransport()
		c.trans = append(c.trans, trans)
		peers = append(peers, addr)
	}

	// Wire the transports together
	c.FullyConnect()

	// Create all the rafts
	for i := 0; i < n; i++ {
		if conf == nil {
			conf = inmemConfig()
		}
		store := c.stores[i]
		trans := c.trans[i]
		peerStore := &StaticPeers{peers}

		raft, err := NewRaft(conf, c.fsms[i], store, store, peerStore, trans)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		c.rafts = append(c.rafts, raft)
	}

	return c
}

func TestRaft_TripleNode(t *testing.T) {
	// Make the cluster
	c := MakeCluster(3, t, nil)
	defer c.Close()

	// Should be one leader
	leader := c.Leader()

	// Should be able to apply
	future := leader.Apply([]byte("test"), time.Millisecond)
	if err := future.Error(); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Wait for replication
	time.Sleep(3 * time.Millisecond)

	// Check that it is applied to the FSM
	for _, fsm := range c.fsms {
		if len(fsm.logs) != 1 {
			t.Fatalf("did not apply to FSM!")
		}
	}
}

func TestRaft_LeaderFail(t *testing.T) {
	// Make the cluster
	c := MakeCluster(3, t, nil)
	defer c.Close()

	// Should be one leader
	leader := c.Leader()

	// Should be able to apply
	future := leader.Apply([]byte("test"), time.Millisecond)
	if err := future.Error(); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Wait for replication
	time.Sleep(3 * time.Millisecond)

	// Disconnect the leader now
	log.Printf("[INFO] Disconnecting %v", leader)
	c.Disconnect(leader.localAddr)

	// Wait for two leaders
	limit := time.Now().Add(100 * time.Millisecond)
	var leaders []*Raft
	for time.Now().Before(limit) && len(leaders) != 2 {
		time.Sleep(10 * time.Millisecond)
		leaders = c.GetInState(Leader)
	}
	if len(leaders) != 2 {
		t.Fatalf("expected two leader: %v", leaders)
	}

	// Get the 'new' leader
	var newLead *Raft
	if leaders[0] == leader {
		newLead = leaders[1]
	} else {
		newLead = leaders[0]
	}

	// Ensure the term is greater
	if newLead.getCurrentTerm() <= leader.getCurrentTerm() {
		t.Fatalf("expected newer term!")
	}

	// Apply should work not work on old leader
	future1 := leader.Apply([]byte("fail"), time.Millisecond)

	// Apply should work on newer leader
	future2 := newLead.Apply([]byte("apply"), time.Millisecond)

	// Future2 should work
	if err := future2.Error(); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Reconnect the networks
	log.Printf("[INFO] Reconnecting %v", leader)
	c.FullyConnect()

	// Future1 should fail
	if err := future1.Error(); err != LeadershipLost {
		t.Fatalf("err: %v", err)
	}

	// Wait for log replication
	time.Sleep(10 * time.Millisecond)

	// Check two entries are applied to the FSM
	for _, fsm := range c.fsms {
		if len(fsm.logs) != 2 {
			t.Fatalf("did not apply both to FSM! %v", fsm.logs)
		}
		if bytes.Compare(fsm.logs[0], []byte("test")) != 0 {
			t.Fatalf("first entry should be 'test'")
		}
		if bytes.Compare(fsm.logs[1], []byte("apply")) != 0 {
			t.Fatalf("second entry should be 'apply'")
		}
	}
}

func TestRaft_BehindFollower(t *testing.T) {
	// Make the cluster
	c := MakeCluster(3, t, nil)
	defer c.Close()

	// Disconnect one follower
	followers := c.GetInState(Follower)
	behind := followers[0]
	c.Disconnect(behind.localAddr)

	// Commit a lot of things
	leader := c.Leader()
	var future ApplyFuture
	for i := 0; i < 100; i++ {
		future = leader.Apply([]byte(fmt.Sprintf("test%d", i)), 0)
	}

	// Wait for the last future to apply
	if err := future.Error(); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Reconnect the behind node
	c.FullyConnect()

	// Wait for replication
	time.Sleep(100 * time.Millisecond)

	// Ensure all the logs are the same
	c.EnsureSame(t)
}

func TestRaft_ApplyNonLeader(t *testing.T) {
	// Make the cluster
	c := MakeCluster(5, t, nil)
	defer c.Close()

	// Wait for a leader
	c.Leader()
	time.Sleep(10 * time.Millisecond)

	// Try to apply to them
	followers := c.GetInState(Follower)
	if len(followers) != 4 {
		t.Fatalf("Expected 4 followers")
	}
	follower := followers[0]

	// Try to apply
	future := follower.Apply([]byte("test"), time.Millisecond)

	if future.Error() != NotLeader {
		t.Fatalf("should not apply on follower")
	}

	// Should be cached
	if future.Error() != NotLeader {
		t.Fatalf("should not apply on follower")
	}
}

func TestRaft_ApplyConcurrent(t *testing.T) {
	// Make the cluster
	conf := inmemConfig()
	conf.HeartbeatTimeout = 100 * time.Millisecond
	c := MakeCluster(3, t, conf)
	defer c.Close()

	// Wait for a leader
	leader := c.Leader()
	for i := 0; i < 100; i++ {
		go func(i int) {
			future := leader.Apply([]byte(fmt.Sprintf("test%d", i)), 0)
			if err := future.Error(); err != nil {
				t.Fatalf("err: %v", err)
			}
		}(i)
	}

	// Wait for replication
	time.Sleep(50 * time.Millisecond)

	// Check the FSMs
	c.EnsureSame(t)
}
