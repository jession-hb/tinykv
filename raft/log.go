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

import pb "github.com/pingcap-incubator/tinykv/proto/pkg/eraftpb"

// RaftLog manage the log entries, its struct look like:
//
//	snapshot/first.....applied....committed....stabled.....last
//	--------|------------------------------------------------|
//	                          log entries
//
// for simplify the RaftLog implement should manage all log entries
// that not truncated
type RaftLog struct {
	// storage contains all stable entries since the last snapshot.
	storage Storage

	// committed is the highest log position that is known to be in
	// stable storage on a quorum of nodes.
	committed uint64

	// applied is the highest log position that the application has
	// been instructed to apply to its state machine.
	// Invariant: applied <= committed
	applied uint64

	// log entries with index <= stabled are persisted to storage.
	// It is used to record the logs that are not persisted by storage yet.
	// Everytime handling `Ready`, the unstabled logs will be included.
	stabled uint64

	// all entries that have not yet compact.
	entries []pb.Entry

	// the incoming unstable snapshot, if any.
	// (Used in 2C)
	pendingSnapshot *pb.Snapshot

	// Your Data Here (2A).
}

// newLog returns log using the given storage. It recovers the log
// to the state that it just commits and applies the latest snapshot.
func newLog(storage Storage) *RaftLog {
	// Your Code Here (2A).
	firstIndex, err := storage.FirstIndex()
	if err != nil {
		panic(err)
	}
	lastIndex, err := storage.LastIndex()
	if err != nil {
		panic(err)
	}
	hardState, _, err := storage.InitialState()
	if err != nil {
		panic(err)
	}

	entries, err := storage.Entries(firstIndex, lastIndex+1)
	if err != nil {
		panic(err)
	}

	return &RaftLog{
		storage:         storage,
		committed:       hardState.Commit,
		applied:         firstIndex - 1,
		stabled:         lastIndex,
		entries:         entries,
		pendingSnapshot: nil,
	}
}

// We need to compact the log entries in some point of time like
// storage compact stabled log entries prevent the log entries
// grow unlimitedly in memory
func (l *RaftLog) maybeCompact() {
	// 通过storage接口获得的FirstIndex是已经被compact（是指一些已经提交且存储在大多数节点的磁盘中的数据）
	// 掉的log的下一个index，但是raftLog的firstIndex可能还没有更新，仍是之前的值
	// 所以会导致一部分数据同时存在在磁盘和内存中，浪费内存空间
	// 因此，如果raftLog的firstIndex小于storage.FirstIndex()，说明有log被compact掉了
	// 那么就需要更新raftLog的firstIndex，并且丢弃已经被compact掉的log
	firstIndex, err := l.storage.FirstIndex()
	if err != nil {
		panic(err)
	}
	if len(l.entries) > 0 {
		if firstIndex > l.LastIndex() {
			l.entries = nil
		} else if firstIndex >= l.FirstIndex() {
			l.entries = l.entries[firstIndex-l.FirstIndex():]
		}
	}
	// Your Code Here (2C).
}

// allEntries return all the entries not compacted.
// note, exclude any dummy entries from the return value.
// note, this is one of the test stub functions you need to implement.
func (l *RaftLog) allEntries() []pb.Entry {
	// Your Code Here (2A).
	firstIndex, err := l.storage.FirstIndex()
	if err != nil {
		panic(err)
	}
	entries := make([]pb.Entry, 0)
	if firstIndex <= l.stabled {
		entries, err = l.storage.Entries(firstIndex, l.stabled+1)
		if err != nil {
			panic(err)
		}
	}
	entries = append(entries, l.unstableEntries()...)
	return entries
}

// unstableEntries return all the unstable entries
func (l *RaftLog) unstableEntries() []pb.Entry {
	// Your Code Here (2A).
	if len(l.entries) == 0 {
		return []pb.Entry{}
	}
	if l.stabled < l.FirstIndex() {
		return l.entries
	}
	start := l.stabled - l.FirstIndex() + 1
	if start >= uint64(len(l.entries)) {
		return []pb.Entry{}
	}
	return l.entries[start:]
}

// nextEnts returns all the committed but not applied entries
func (l *RaftLog) nextEnts() (ents []pb.Entry) {
	// Your Code Here (2A).
	if len(l.entries) > 0 {
		if l.applied >= l.FirstIndex()-1 && l.committed >= l.FirstIndex()-1 && l.applied < l.committed && l.committed <= l.LastIndex() {
			return l.entries[l.applied-l.FirstIndex()+1 : l.committed-l.FirstIndex()+1]
		}
	}
	return make([]pb.Entry, 0)
}

func (l *RaftLog) FirstIndex() uint64 {
	if len(l.entries) > 0 {
		// 如果当前持有的日志不为空，就直接返回一个日志的index
		return l.entries[0].Index
	}
	// 否则当前状态要么是集群刚启动，要么是snapshot刚被安装或者日志被compact掉了
	// 这几种情况下都需要通过storage接口获取firstIndex
	index, _ := l.storage.FirstIndex()
	return index
}

// LastIndex return the last index of the log entries
func (l *RaftLog) LastIndex() uint64 {
	// Your Code Here (2A).
	if len(l.entries) > 0 {
		return l.entries[len(l.entries)-1].Index
	}
	return l.stabled
}

// Term return the term of the entry in the given index
func (l *RaftLog) Term(i uint64) (uint64, error) {
	// Your Code Here (2A).
	if !IsEmptySnap(l.pendingSnapshot) && i == l.pendingSnapshot.Metadata.Index {
		return l.pendingSnapshot.Metadata.Term, nil
	}

	if len(l.entries) > 0 {
		if i >= l.FirstIndex() && i <= l.LastIndex() {
			return l.entries[i-l.FirstIndex()].Term, nil
		}
	}

	term, err := l.storage.Term(i)
	if err != nil {
		return term, err
	}

	return 0, nil
}

func (l *RaftLog) appliedTo(i uint64) {
	l.applied = i
}

func (l *RaftLog) committedTo(i uint64) {
	if l.committed < i {
		l.committed = i
	}
}
