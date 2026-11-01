package control

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A recovery resave must be classifiable as a record persistence failure rather
// than a generic startup error. Closing the store first makes the resave return
// ErrClosed without relying on filesystem permissions.
func TestRecoverySaveFailureWrapsErrRecordWrite(t *testing.T) {
	s, err := openStore(t.TempDir())
	require.NoError(t, err)
	record := typedStoreRecord(t)
	require.NoError(t, s.save(record))
	require.NoError(t, s.close())
	err = s.recover()
	require.ErrorIs(t, err, ErrRecordWrite, "recovery save error = %v, want ErrRecordWrite", err)
	require.ErrorIs(t, err, ErrClosed, "recovery save lost the underlying ErrClosed: %v", err)
}
