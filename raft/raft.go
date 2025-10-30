// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package raft

import (
	"errors"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/pingcap-incubator/tinykv/log"
	pb "github.com/pingcap-incubator/tinykv/proto/pkg/eraftpb"
)

// None is a placeholder node ID used when there is no leader.
const None uint64 = 0

// StateType represents the role of a node in a cluster.
type StateType uint64

const (
	StateFollower StateType = iota
	StateCandidate
	StateLeader
)

var stmap = [...]string{
	"StateFollower",
	"StateCandidate",
	"StateLeader",
}

func (st StateType) String() string {
	return stmap[uint64(st)]
}

// ErrProposalDropped is returned when the proposal is ignored by some cases,
// so that the proposer can be notified and fail fast.
var ErrProposalDropped = errors.New("raft proposal dropped")

// 用于随机数
type lockedRand struct {
	mu   sync.Mutex
	rand *rand.Rand
}

var globalRand = &lockedRand{
	rand: rand.New(rand.NewSource(time.Now().UnixNano())),
}

func (r *lockedRand) Intn(n int) int {
	r.mu.Lock()
	v := r.rand.Intn(n)
	r.mu.Unlock()
	return v
}

// Config contains the parameters to start a raft.
type Config struct {
	// ID is the identity of the local raft. ID cannot be 0.
	ID uint64

	// peers contains the IDs of all nodes (including self) in the raft cluster. It
	// should only be set when starting a new raft cluster. Restarting raft from
	// previous configuration will panic if peers is set. peer is private and only
	// used for testing right now.
	peers []uint64

	// ElectionTick is the number of Node.Tick invocations that must pass between
	// elections. That is, if a follower does not receive any message from the
	// leader of current term before ElectionTick has elapsed, it will become
	// candidate and start an election. ElectionTick must be greater than
	// HeartbeatTick. We suggest ElectionTick = 10 * HeartbeatTick to avoid
	// unnecessary leader switching.
	ElectionTick int
	// HeartbeatTick is the number of Node.Tick invocations that must pass between
	// heartbeats. That is, a leader sends heartbeat messages to maintain its
	// leadership every HeartbeatTick ticks.
	HeartbeatTick int

	// Storage is the storage for raft. raft generates entries and states to be
	// stored in storage. raft reads the persisted entries and states out of
	// Storage when it needs. raft reads out the previous state and configuration
	// out of storage when restarting.
	Storage Storage
	// Applied is the last applied index. It should only be set when restarting
	// raft. raft will not return entries to the application smaller or equal to
	// Applied. If Applied is unset when restarting, raft might return previous
	// applied entries. This is a very application dependent configuration.
	Applied uint64
}

func (c *Config) validate() error {
	if c.ID == None {
		return errors.New("cannot use none as id")
	}

	if c.HeartbeatTick <= 0 {
		return errors.New("heartbeat tick must be greater than 0")
	}

	if c.ElectionTick <= c.HeartbeatTick {
		return errors.New("election tick must be greater than heartbeat tick")
	}

	if c.Storage == nil {
		return errors.New("storage cannot be nil")
	}

	return nil
}

// Progress represents a follower’s progress in the view of the leader. Leader maintains
// progresses of all followers, and sends entries to the follower based on its progress.
type Progress struct {
	Match, Next uint64
}

type Raft struct {
	id uint64

	Term uint64
	Vote uint64

	// the log
	RaftLog *RaftLog

	// log replication progress of each peers
	Prs map[uint64]*Progress

	// this peer's role
	State StateType

	// votes records,有哪些节点给自己投了票
	votes map[uint64]bool // key: peerID, value: true (vote granted); false (vote rejected)

	// msgs need to send
	msgs []pb.Message

	// the leader id
	Lead uint64

	// heartbeat interval, should send
	heartbeatTimeout int
	// baseline of election interval
	electionTimeout int
	// number of ticks since it reached last heartbeatTimeout.
	// only leader keeps heartbeatElapsed.
	heartbeatElapsed int // 心跳计时；当 heartbeatElapsed 到达 heartbeatTimeout 时，说明 Leader 该发起心跳了，随后重置
	// Ticks since it reached last electionTimeout when it is leader or candidate.
	// Number of ticks since it reached last electionTimeout or received a
	// valid message from current leader when it is a follower.
	electionElapsed int // 选举计时；每次tick都将选举计数+1，当Follower收到Leader心跳的时候会将electionElapsed清0。如果Follower收不到Leader的心跳，electionElapsed就会一直加到超过选举超时，就发起选举，随后重置

	// leadTransferee is id of the leader transfer target when its value is not zero.
	// Follow the procedure defined in section 3.10 of Raft phd thesis.
	// (https://web.stanford.edu/~ouster/cgi-bin/papers/OngaroPhD.pdf)
	// (Used in 3A leader transfer)
	leadTransferee uint64

	// Only one conf change may be pending (in the log, but not yet
	// applied) at a time. This is enforced via PendingConfIndex, which
	// is set to a value >= the log index of the latest pending
	// configuration change (if any). Config changes are only allowed to
	// be proposed if the leader's applied index is greater than this
	// value.
	// (Used in 3A conf change)
	PendingConfIndex uint64
}

// newRaft return a raft peer with the given config
func newRaft(c *Config) *Raft {
	if err := c.validate(); err != nil {
		panic(err.Error())
	}
	raftLog := newLog(c.Storage)
	hs, cs, err := c.Storage.InitialState()
	if err != nil {
		panic(err)
	}

	if c.peers == nil {
		c.peers = cs.Nodes
	}
	prs := make(map[uint64]*Progress)
	for _, pr := range c.peers {
		prs[pr] = &Progress{
			Match: 0,
			Next:  0,
		}
	}

	raft := &Raft{
		id:               c.ID,
		Term:             hs.Term,
		Vote:             hs.Vote,
		RaftLog:          raftLog,
		Prs:              prs,
		State:            StateFollower,
		votes:            make(map[uint64]bool),
		Lead:             None,
		heartbeatTimeout: c.HeartbeatTick,
		electionTimeout:  c.ElectionTick,
		leadTransferee:   0,
	}
	if c.Applied > 0 {
		raftLog.appliedTo(c.Applied)
	}
	return raft
}

// sendAppend sends an append RPC with new entries (if any) and the
// current commit index to the given peer. Returns true if a message was sent.
func (r *Raft) sendAppend(to uint64) {
	// Your Code Here (2A).
	pr, ok := r.Prs[to]
	if !ok {
		return
	}
	prevLogIndex := pr.Next - 1
	prevLogTerm, err := r.RaftLog.Term(prevLogIndex)
	if err != nil {
		return
	}

	entries := make([]*pb.Entry, 0)
	for i := pr.Next; i < r.RaftLog.LastIndex()+1; i++ {
		entries = append(entries, &r.RaftLog.entries[i-r.RaftLog.FirstIndex()])
	}

	msg := pb.Message{
		MsgType: pb.MessageType_MsgAppend,
		To:      to,
		Term:    r.Term,
		From:    r.id,
		Index:   prevLogIndex,
		LogTerm: prevLogTerm,
		Entries: entries,
		Commit:  min(r.RaftLog.committed, r.Prs[to].Match),
	}
	r.msgs = append(r.msgs, msg)
	log.DPrintfRaft("%x send append to %x, prevLogIndex:%d, prevLogTerm:%d, entries:%v, commit:%d\n", r.id, to, prevLogIndex, prevLogTerm, entries, msg.Commit)
}

func (r *Raft) sendAppendResponse(reject bool, to uint64, index uint64) {
	msg := pb.Message{
		MsgType: pb.MessageType_MsgAppendResponse,
		Term:    r.Term,
		To:      to,
		From:    r.id,
		Reject:  reject,
		Index:   index,
	}
	r.msgs = append(r.msgs, msg)
	log.DPrintfRaft("%x send append response to %x, reject:%v, index:%d\n", r.id, to, reject, index)
}

// sendHeartbeat sends a heartbeat RPC to the given peer.
func (r *Raft) sendHeartbeat(to uint64) {
	// Your Code Here (2A).
	if _, ok := r.Prs[to]; !ok {
		return
	}
	if r.State != StateLeader {
		return
	}
	msg := pb.Message{
		MsgType: pb.MessageType_MsgHeartbeat,
		Term:    r.Term,
		To:      to,
		Commit:  min(r.RaftLog.committed, r.Prs[to].Match),
		From:    r.id,
	}
	r.msgs = append(r.msgs, msg)
}

func (r *Raft) sendHeartbeatResponse(to uint64) {
	msg := pb.Message{
		MsgType: pb.MessageType_MsgHeartbeatResponse,
		Term:    r.Term,
		To:      to,
		From:    r.id,
		Commit:  r.RaftLog.committed,
	}
	r.msgs = append(r.msgs, msg)
}

func (r *Raft) sendAllRequestVote() {
	// Your Code Here (2A).
	for id := range r.Prs {
		if id == r.id {
			continue
		}
		r.sendRequestVote(id)
	}
}

func (r *Raft) sendRequestVote(to uint64) {
	_, ok := r.Prs[to]
	if !ok {
		return
	}
	// 论文中的最后一条日志条目的索引和任期，用于判断是否候选者的日志至少和接收者的一样新
	lastLogIndex := r.RaftLog.LastIndex()
	lastLogTerm, err := r.RaftLog.Term(lastLogIndex)
	if err != nil {
		return
	}
	msg := pb.Message{
		MsgType: pb.MessageType_MsgRequestVote,
		To:      to,
		Term:    r.Term,
		LogTerm: lastLogTerm,
		Index:   lastLogIndex,
		From:    r.id,
	}
	r.msgs = append(r.msgs, msg)
	log.DPrintfRaft("%x send requestVote to %x", r.id, to)
}

func (r *Raft) sendRequestVoteResponse(reject bool, to uint64) {
	msg := pb.Message{
		MsgType: pb.MessageType_MsgRequestVoteResponse,
		To:      to,
		Reject:  reject,
		Term:    r.Term,
		From:    r.id,
	}
	r.msgs = append(r.msgs, msg)
	log.DPrintfRaft("%x send requestVote response to %x, reject:%v\n", r.id, to, reject)
}

// tick advances the internal logical clock by a single tick.
func (r *Raft) tick() {
	// Your Code Here (2A).
	r.electionElapsed++
	switch r.State {
	case StateFollower:
		if r.electionElapsed >= r.electionTimeout {
			r.electionElapsed = 0
			err := r.Step(pb.Message{MsgType: pb.MessageType_MsgHup})
			if err != nil {
				return
			}
		}
	case StateCandidate:
		if r.electionElapsed >= r.electionTimeout {
			r.electionElapsed = 0
			err := r.Step(pb.Message{MsgType: pb.MessageType_MsgHup})
			if err != nil {
				return
			}
		}
	case StateLeader:
		r.heartbeatElapsed++
		if r.heartbeatElapsed >= r.heartbeatTimeout {
			r.heartbeatElapsed = 0
			err := r.Step(pb.Message{MsgType: pb.MessageType_MsgBeat})
			if err != nil {
				return
			}
		}
	}
}

// becomeFollower transform this peer's state to Follower
func (r *Raft) becomeFollower(term uint64, lead uint64) {
	// Your Code Here (2A).
	r.Lead = lead
	r.State = StateFollower
	r.reset(term)
	log.DPrintfRaft("%x become follower at term %d\n", r.id, r.Term)
}

// becomeCandidate transform this peer's state to candidate
func (r *Raft) becomeCandidate() {
	// Your Code Here (2A).
	r.State = StateCandidate
	r.reset(r.Term + 1)
	r.Vote = r.id
	r.votes[r.id] = true
	log.DPrintfRaft("%x become candidate at term %d\n", r.id, r.Term)
}

// becomeLeader transform this peer's state to leader
func (r *Raft) becomeLeader() {
	// Your Code Here (2A).
	// NOTE: Leader should propose a noop entry on its term
	if r.State == StateFollower && len(r.Prs) != 1 {
		log.Panic("invalid transition [follower -> leader]")
	}
	r.reset(r.Term)
	r.State = StateLeader
	r.Lead = r.id

	lastIndex := r.RaftLog.LastIndex()
	for id := range r.Prs {
		r.Prs[id].Next = lastIndex + 1
		r.Prs[id].Match = 0
	}

	// 提交一个空的noop日志
	r.RaftLog.entries = append(r.RaftLog.entries, pb.Entry{Term: r.Term, Index: lastIndex + 1})
	r.Prs[r.id].Match = r.RaftLog.LastIndex()
	r.Prs[r.id].Next = r.RaftLog.LastIndex() + 1

	log.DPrintfRaft("%x become leader at term %d\n", r.id, r.Term)

	// 发送追加日志
	for id := range r.Prs {
		if id != r.id {
			r.sendAppend(id)
		}
	}

	// 更新提交日志索引，以便正常进行客户端交互
	r.updateCommitIndex()
}

// Step the entrance of handle message, see `MessageType`
// on `eraftpb.proto` for what msgs should be handled
func (r *Raft) Step(m pb.Message) error {
	// Your Code Here (2A).
	var err error = nil
	switch r.State {
	case StateFollower:
		err = r.stepFollower(m)
	case StateCandidate:
		err = r.stepCandidate(m)
	case StateLeader:
		err = r.stepLeader(m)
	}
	return err
}

func (r *Raft) stepFollower(m pb.Message) error {
	var err error = nil
	switch m.MsgType {
	case pb.MessageType_MsgHup:
		// 等待超时，发起选举
		if _, ok := r.Prs[r.id]; ok {
			r.startElection()
		}
	case pb.MessageType_MsgBeat:
	case pb.MessageType_MsgPropose:
		err = ErrProposalDropped
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
	case pb.MessageType_MsgAppendResponse:
	case pb.MessageType_MsgRequestVote:
		r.handleRequestVote(m)
	case pb.MessageType_MsgRequestVoteResponse:
	case pb.MessageType_MsgSnapshot:
		r.handleSnapshot(m)
	case pb.MessageType_MsgHeartbeat:
		r.handleHeartbeat(m)
	case pb.MessageType_MsgHeartbeatResponse:
	case pb.MessageType_MsgTransferLeader:
		if r.Lead != None {
			m.To = r.Lead
			r.msgs = append(r.msgs, m)
		}
	case pb.MessageType_MsgTimeoutNow:
		r.electionElapsed = 0
		r.startElection()
	}
	return err
}

func (r *Raft) stepCandidate(m pb.Message) error {
	var err error = nil
	switch m.MsgType {
	case pb.MessageType_MsgHup:
		// 选举超时，重新发起选举
		if _, ok := r.Prs[r.id]; ok {
			r.startElection()
		}
	case pb.MessageType_MsgBeat:
	case pb.MessageType_MsgPropose:
		err = ErrProposalDropped
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
	case pb.MessageType_MsgAppendResponse:
	case pb.MessageType_MsgRequestVote:
		r.handleRequestVote(m)
	case pb.MessageType_MsgRequestVoteResponse:
		// 判断是否自己已经获得了过半数的选票
		total := len(r.Prs)
		agree := 0
		refuse := 0
		r.votes[m.From] = !m.Reject
		for _, v := range r.votes {
			if v {
				agree++
			} else {
				refuse++
			}
		}
		if agree > total/2 {
			// 成为Leader
			r.becomeLeader()
		} else if refuse > total/2 {
			// 变成Follower
			r.becomeFollower(r.Term, None)
		}
	case pb.MessageType_MsgSnapshot:
		r.handleSnapshot(m)
	case pb.MessageType_MsgHeartbeat:
		r.handleHeartbeat(m)
	case pb.MessageType_MsgHeartbeatResponse:
	case pb.MessageType_MsgTransferLeader:
		if r.Lead != None {
			m.To = r.Lead
			r.msgs = append(r.msgs, m)
		}
	case pb.MessageType_MsgTimeoutNow:
		r.electionElapsed = 0
		r.startElection()
	}
	return err
}

func (r *Raft) stepLeader(m pb.Message) error {
	var err error = nil
	switch m.MsgType {
	case pb.MessageType_MsgHup:
	case pb.MessageType_MsgBeat:
		for id := range r.Prs {
			if id == r.id {
				continue
			}
			r.sendHeartbeat(id)
		}
	case pb.MessageType_MsgPropose:
		// 不发生领导权转移时，处理提案
		if r.leadTransferee == None {
			r.handlePropose(m)
		} else {
			err = ErrProposalDropped
		}
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
	case pb.MessageType_MsgAppendResponse:
		r.handleAppendResponse(m)
	case pb.MessageType_MsgRequestVote:
		r.handleRequestVote(m)
	case pb.MessageType_MsgRequestVoteResponse:
	case pb.MessageType_MsgHeartbeat:
		r.handleHeartbeat(m)
	case pb.MessageType_MsgHeartbeatResponse:
		r.handleHeartbeatResponse(m)
	case pb.MessageType_MsgTransferLeader:
		r.handleTransferLeader(m)
	case pb.MessageType_MsgTimeoutNow:
		r.electionElapsed = 0
		r.startElection()
	}
	return err
}

func (r *Raft) startElection() {
	// Your Code Here (2A).
	// 首先判断节点是否在集群中
	if _, ok := r.Prs[r.id]; !ok {
		return
	}
	if len(r.Prs) == 1 {
		// 集群中只有自己，直接成为Leader
		r.becomeLeader()
		r.Term++
	} else {
		r.becomeCandidate()
		r.sendAllRequestVote()
	}
}

func (r *Raft) handleRequestVote(m pb.Message) {
	// 消息任期更大，直接变成Follower
	log.DPrintfRaft("%x receive requestVote from %x\n", r.id, m.From)
	if r.Term < m.Term {
		r.Vote = None
		r.Term = m.Term
		r.becomeFollower(m.Term, None)
	}
	if m.Term < r.Term {
		// 直接拒绝
		r.sendRequestVoteResponse(true, m.From)
		return
	}
	// 判断是否已经投过票
	if r.Vote != None && r.Vote != m.From {
		// 已经投过票，拒绝
		r.sendRequestVoteResponse(true, m.From)
		return
	} else {
		// 判断候选者的日志是否至少和自己的一样新
		lastLogIndex := r.RaftLog.LastIndex()
		lastLogTerm, err := r.RaftLog.Term(lastLogIndex)
		if err != nil {
			return
		}
		if m.LogTerm < lastLogTerm || (m.LogTerm == lastLogTerm && m.Index < lastLogIndex) {
			// 候选者的日志不够新，拒绝
			r.sendRequestVoteResponse(true, m.From)
			return
		}
		// 同意投票
		r.Vote = m.From
		r.sendRequestVoteResponse(false, m.From)
	}
}

func (r *Raft) handlePropose(m pb.Message) {
	if r.State != StateLeader {
		return
	}

	log.DPrintfRaft("%x receive propose from %x\n", r.id, m.From)
	r.appendEntry(m.Entries)
	// 给所有Follower发送AppendEntries
	for id := range r.Prs {
		if id == r.id {
			continue
		}
		r.sendAppend(id)
	}
	if len(r.Prs) == 1 {
		r.RaftLog.committedTo(r.Prs[r.id].Match)
	}
}

func (r *Raft) handleAppendResponse(m pb.Message) {
	if r.State != StateLeader {
		return
	}

	log.DPrintfRaft("%x receive appendResponse from %x\n", r.id, m.From)
	if m.Reject {
		// 回退Next
		r.Prs[m.From].Next = m.Index + 1
		r.sendAppend(m.From)
		return
	}

	// 更新Match和Next
	r.Prs[m.From].Match = m.Index
	r.Prs[m.From].Next = m.Index + 1

	// 更新提交日志索引
	oldCom := r.RaftLog.committed
	r.updateCommitIndex()
	if r.RaftLog.committed != oldCom {
		for id := range r.Prs {
			if id != r.id {
				r.sendAppend(id)
			}
		}
	}

	// 处理领导权转移
	if m.From == r.leadTransferee {
		r.Step(pb.Message{MsgType: pb.MessageType_MsgTransferLeader, From: m.From})
	}
}

// handleAppendEntries handle AppendEntries RPC request
// 该函数用于处理来自Leader的日志追加请求，注意是entries，所以可能包含多条日志
// 日志一致性的判断主要看发送过来的entries之前的一条日志prevLogIndex和prevLogTerm是否存在在当前节点的log中
func (r *Raft) handleAppendEntries(m pb.Message) {
	// Your Code Here (2A).
	log.DPrintfRaft("%x receive append from %x\n", r.id, m.From)
	if r.Term <= m.Term {
		r.Term = m.Term
		if r.State != StateFollower {
			r.becomeFollower(r.Term, None)
		}
	}
	if r.State == StateLeader {
		return
	}
	if m.Term < r.Term {
		// 直接拒绝
		r.sendAppendResponse(true, m.From, r.RaftLog.LastIndex())
		return
	}

	// 更新Leader信息
	if m.From != r.Lead {
		r.Lead = m.From
	}
	prevLogIndex := m.Index
	prevLogTerm := m.LogTerm

	// 判断日志一致性
	if prevLogIndex > r.RaftLog.LastIndex() {
		// 直接拒绝
		r.sendAppendResponse(true, m.From, r.RaftLog.LastIndex())
		return
	}

	if tmpTerm, _ := r.RaftLog.Term(prevLogIndex); tmpTerm != prevLogTerm {
		// 直接拒绝，需要append的日志前一条索引对应的任期不一致
		// 所以返回prevLogIndex-1，表示冲突发生在该索引处
		r.sendAppendResponse(true, m.From, prevLogIndex-1)
		return
	}

	for _, en := range m.Entries {
		index := en.Index
		oldTerm, err := r.RaftLog.Term(index)
		if index > r.RaftLog.LastIndex() {
			r.RaftLog.entries = append(r.RaftLog.entries, *en)
		} else if oldTerm != en.Term || err != nil {
			// 日志冲突，删除当前日志之后的所有日志
			if index < r.RaftLog.FirstIndex() {
				r.RaftLog.entries = make([]pb.Entry, 0)
			} else {
				r.RaftLog.entries = r.RaftLog.entries[:index-r.RaftLog.FirstIndex()]
			}
			r.RaftLog.stabled = min(r.RaftLog.stabled, index-1)
			r.RaftLog.entries = append(r.RaftLog.entries, *en)
		}
	}

	r.sendAppendResponse(false, m.From, r.RaftLog.LastIndex())

	// 更新提交日志索引
	if m.Commit > r.RaftLog.committed {
		r.RaftLog.committed = min(m.Commit, r.RaftLog.LastIndex())
	}
}

// handleHeartbeat handle Heartbeat RPC request
func (r *Raft) handleHeartbeat(m pb.Message) {
	// Your Code Here (2A).
	log.DPrintfRaft("%x receive heartbeat from %x\n", r.id, m.From)
	if r.Term <= m.Term {
		r.Term = m.Term
		if r.State != StateFollower {
			r.becomeFollower(r.Term, None)
		}
	}
	if m.From != r.Lead {
		r.Lead = m.From
	}

	r.electionElapsed = 0
	r.sendHeartbeatResponse(m.From)
}

func (r *Raft) handleHeartbeatResponse(m pb.Message) {
	if r.State != StateLeader {
		return
	}

	if r.Term < m.Term {
		r.Term = m.Term
		if r.State != StateFollower {
			r.becomeFollower(r.Term, None)
		}
	}
	log.DPrintfRaft("%x receive heartbeatResponse from %x\n", r.id, m.From)
	if m.Commit < r.RaftLog.committed {
		r.sendAppend(m.From)
	}
}

func (r *Raft) handleTransferLeader(m pb.Message) {
}

// handleSnapshot handle Snapshot RPC request
func (r *Raft) handleSnapshot(m pb.Message) {
	// Your Code Here (2C).
}

// addNode add a new node to raft group
func (r *Raft) addNode(id uint64) {
	// Your Code Here (3A).
}

// removeNode remove a node from raft group
func (r *Raft) removeNode(id uint64) {
	// Your Code Here (3A).
}

func (r *Raft) appendEntry(ents []*pb.Entry) {
	lastIndex := r.RaftLog.LastIndex()
	for i, ent := range ents {
		ent.Index = lastIndex + uint64(i) + 1
		ent.Term = r.Term
		r.RaftLog.entries = append(r.RaftLog.entries, *ent)
	}
	r.Prs[r.id].Match = r.RaftLog.LastIndex()
	r.Prs[r.id].Next = r.RaftLog.LastIndex() + 1
}

func (r *Raft) reset(term uint64) {
	r.Term = term
	r.Vote = None
	r.Lead = None
	r.electionElapsed = 0
	r.heartbeatElapsed = 0
	r.resetRandomizedElectionTimeout()
	r.leadTransferee = None
	r.votes = make(map[uint64]bool)
}

func (r *Raft) resetRandomizedElectionTimeout() {
	rand := globalRand.Intn(r.electionTimeout)
	r.electionTimeout = r.electionTimeout + rand
	// 限制在10~20之间
	for r.electionTimeout >= 20 {
		r.electionTimeout -= 10
	}
}

func (r *Raft) updateCommitIndex() {
	match := make(uint64Slice, len(r.Prs))
	i := 0
	for _, pr := range r.Prs {
		match[i] = pr.Match
		i++
	}
	// 对match进行排序
	sort.Sort(match)
	N := match[(len(r.Prs)-1)/2]
	for ; N > r.RaftLog.committed; N-- {
		if term, _ := r.RaftLog.Term(N); term == r.Term {
			break
		}
	}
	r.RaftLog.committedTo(N)
}
