package control

import (
	"context"
	"errors"

	"github.com/dongwlin/rhodeck/packages/control/worker"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

// Doctor loads an explicit installation in a supervised child and reports its
// immutable snapshot. It never connects, appends tasks or performs device work.
// Initialization may create Core logs under the caller's writable UserDir.
func Doctor(ctx context.Context, cfg Config, init protocol.InitializeRequest) (info protocol.RuntimeInfo, err error) {
	if ctx == nil {
		return info, errors.New("control: doctor requires a context")
	}
	if err = validateInitialize(init); err != nil {
		return info, err
	}
	s, err := openStore(cfg.DataDir)
	if err != nil {
		return info, err
	}
	defer func() { err = errors.Join(err, s.close()) }()
	client, err := worker.Launch(ctx, cfg.Worker)
	if err != nil {
		return info, err
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range client.Events() {
		}
	}()
	defer func() {
		closeErr := client.Close(context.Background())
		<-drained
		err = errors.Join(err, closeErr, client.Err())
		if fault := client.InstanceErr(); fault != nil {
			err = errors.Join(err, fault)
		}
	}()
	info, err = client.Initialize(ctx, init)
	return info, err
}
