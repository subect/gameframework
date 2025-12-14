package reliable

import (
	"sync"
	"time"
)

func nowMs() int64 { return time.Now().UnixNano() / int64(time.Millisecond) }

func SeqMoreRecent(s1, s2 uint32) bool { return int32(s1-s2) > 0 }

type ReliableReceiver struct {
	mu           sync.Mutex
	received     map[uint32]bool
	lastReceived uint32
	processed    map[uint32]bool
}

func NewReliableReceiver() *ReliableReceiver {
	return &ReliableReceiver{
		received:  make(map[uint32]bool),
		processed: make(map[uint32]bool),
	}
}
func (r *ReliableReceiver) MarkReceived(seq uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.received[seq] = true
	if r.lastReceived == 0 || SeqMoreRecent(seq, r.lastReceived) {
		r.lastReceived = seq
	}
}
func (r *ReliableReceiver) AlreadyProcessed(seq uint32) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.processed[seq]
}
func (r *ReliableReceiver) MarkProcessed(seq uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.processed[seq] = true
}
func (r *ReliableReceiver) BuildAckAndBits() (uint32, uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastReceived == 0 {
		return 0, 0
	}
	ack := r.lastReceived
	var bits uint32
	for i := 0; i < 32; i++ {
		seq := ack - 1 - uint32(i)
		if r.received[seq] {
			bits |= (1 << uint(i))
		}
	}
	return ack, bits
}

type pendingMsg struct {
	Seq        uint32
	Payload    []byte
	LastSentMs int64
	SendCount  int
}

type ReliableSender struct {
	mu            sync.Mutex
	nextSeq       uint32
	pending       map[uint32]*pendingMsg
	nextPacketSeq uint32
}

func NewReliableSender() *ReliableSender {
	return &ReliableSender{
		nextSeq:       1,
		pending:       make(map[uint32]*pendingMsg),
		nextPacketSeq: 1,
	}
}
func (s *ReliableSender) AddPending(payload []byte) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	seq := s.nextSeq
	s.nextSeq++
	cp := append([]byte(nil), payload...)
	s.pending[seq] = &pendingMsg{Seq: seq, Payload: cp}
	return seq
}
func (s *ReliableSender) UpdatePendingSent(seq uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.pending[seq]; ok {
		p.LastSentMs = nowMs()
		p.SendCount++
	}
}
func (s *ReliableSender) GetPendingOlderThan(thresholdMs int64) []*pendingMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := nowMs()
	out := []*pendingMsg{}
	for _, p := range s.pending {
		if p.LastSentMs == 0 || now-p.LastSentMs >= thresholdMs {
			out = append(out, p)
		}
	}
	return out
}
func (s *ReliableSender) ProcessAckFromRemote(ack uint32, bits uint32) []uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	cleared := []uint32{}
	if ack != 0 {
		if _, ok := s.pending[ack]; ok {
			delete(s.pending, ack)
			cleared = append(cleared, ack)
		}
	}
	for i := 0; i < 32; i++ {
		if bits&(1<<uint(i)) != 0 {
			seq := ack - 1 - uint32(i)
			if _, ok := s.pending[seq]; ok {
				delete(s.pending, seq)
				cleared = append(cleared, seq)
			}
		}
	}
	return cleared
}
func (s *ReliableSender) NextPacketSeq() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.nextPacketSeq
	s.nextPacketSeq++
	return v
}
