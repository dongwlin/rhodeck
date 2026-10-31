package control

import (
	"errors"
	"testing"
)

// A recovery resave must be classifiable as a record persistence failure rather
// than a generic startup error. Closing the store first makes the resave return
// ErrClosed without relying on filesystem permissions.
func TestRecoverySaveFailureWrapsErrRecordWrite(t *testing.T) {
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := typedStoreRecord(t)
	if err := s.save(record); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	err = s.recover()
	if !errors.Is(err, ErrRecordWrite) {
		t.Fatalf("recovery save error = %v, want ErrRecordWrite", err)
	}
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("recovery save lost the underlying ErrClosed: %v", err)
	}
}
