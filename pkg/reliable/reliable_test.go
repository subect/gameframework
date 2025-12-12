package reliable

import "testing"

func TestAddPendingAndAck(t *testing.T) {
	s := NewReliableSender()
	r := NewReliableReceiver()

	seq := s.AddPending([]byte{1, 2, 3})
	if seq != 1 {
		t.Fatal("expected seq 1")
	}
	r.MarkReceived(1)
	ack, bits := r.BuildAckAndBits()
	cleared := s.ProcessAckFromRemote(ack, bits)
	if len(cleared) != 1 || cleared[0] != 1 {
		t.Fatalf("pending not cleared: %v", cleared)
	}
}

func TestAckBits(t *testing.T) {
	r := NewReliableReceiver()
	r.MarkReceived(5)
	r.MarkReceived(4)
	r.MarkReceived(2)
	ack, bits := r.BuildAckAndBits()
	if ack != 5 {
		t.Fatal("ack should be 5")
	}
	if bits&(1<<0) == 0 {
		t.Fatal("bit0 for seq4 expected")
	}
	if bits&(1<<2) == 0 {
		t.Fatal("bit2 for seq2 expected")
	}
}
