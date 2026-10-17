package l2

import "sync/atomic"

// IOStats is what the L2 file has asked of its store since it was opened.
//
// It exists to make the access *pattern* visible, not just the volume. Laying a
// region out so that neighbouring blocks are physically adjacent is only worth
// anything if a burst of neighbouring reads turns into one transfer, and that
// shows up in the ratio of Reads to ReadBytes and in SequentialReads — not in
// any throughput number the caller can see, because a miss and a hit made of
// sixteen small transfers both just return data.
//
// The counters are read without a lock, so a counter that moves concurrently
// with the read may or may not be included; it is never counted twice.
type IOStats struct {
	Reads      int64
	ReadBytes  int64
	Writes     int64
	WriteBytes int64
	// SequentialReads counts the reads that began exactly where the previous
	// read ended. Under concurrent reads "previous" is whichever read finished
	// last, so this is a lower bound on how much of the traffic was sequential.
	SequentialReads int64
}

// AvgReadBytes is the average transfer size, or zero when nothing was read.
func (s IOStats) AvgReadBytes() int64 {
	if s.Reads == 0 {
		return 0
	}
	return s.ReadBytes / s.Reads
}

// statStore wraps the platform store and counts what passes through it.
//
// It is installed by Open rather than instrumenting each call site, so a
// transfer cannot be added to the file without being counted, and so the
// counters describe the file's real IO and not a list of the paths someone
// remembered to update.
type statStore struct {
	Store
	reads      atomic.Int64
	readBytes  atomic.Int64
	writes     atomic.Int64
	writeBytes atomic.Int64
	seqReads   atomic.Int64
	// lastReadEnd is where the previous read finished, or -1 before the first
	// one. It is only ever used to classify a read, never to address one.
	lastReadEnd atomic.Int64
}

func newStatStore(s Store) *statStore {
	st := &statStore{Store: s}
	st.lastReadEnd.Store(-1)
	return st
}

// ReadAt implements Store. Only successful transfers are counted, so an IO that
// failed does not inflate the average transfer size.
func (s *statStore) ReadAt(p []byte, off int64) error {
	if err := s.Store.ReadAt(p, off); err != nil {
		return err
	}
	s.reads.Add(1)
	s.readBytes.Add(int64(len(p)))
	if s.lastReadEnd.Swap(off+int64(len(p))) == off {
		s.seqReads.Add(1)
	}
	return nil
}

// WriteAt implements Store.
func (s *statStore) WriteAt(p []byte, off int64) error {
	if err := s.Store.WriteAt(p, off); err != nil {
		return err
	}
	s.writes.Add(1)
	s.writeBytes.Add(int64(len(p)))
	return nil
}

func (s *statStore) stats() IOStats {
	return IOStats{
		Reads:           s.reads.Load(),
		ReadBytes:       s.readBytes.Load(),
		Writes:          s.writes.Load(),
		WriteBytes:      s.writeBytes.Load(),
		SequentialReads: s.seqReads.Load(),
	}
}
