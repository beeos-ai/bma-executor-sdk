//go:build !linux

package sessionmount

// New fails: see ErrUnsupported.
func New(Options) (*Mounter, error) { return nil, ErrUnsupported }

// Probe fails: see ErrUnsupported.
func Probe(string) error { return ErrUnsupported }

// DetachStale has nothing to detach.
func DetachStale(string) error { return nil }

type mounted struct{ view *view }

func mount(*view) (*mounted, error) { return nil, ErrUnsupported }

func (*mounted) unmount() error { return nil }
