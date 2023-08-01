package proxy

import (
	"context"
	"fmt"

	gi "gitlab.com/TitanInd/proxy/proxy-router-v3/internal/interfaces"
	"gitlab.com/TitanInd/proxy/proxy-router-v3/internal/lib"
)

type Pipe struct {
	// state
	sourceToDestTask *lib.Task
	destToSourceTask *lib.Task

	// deps
	source            StratumReadWriter // initiator of the communication, miner
	dest              StratumReadWriter // receiver of the communication, pool
	sourceInterceptor Interceptor
	destInterceptor   Interceptor

	log gi.ILogger
}

// NewPipe creates a new pipe between source and dest, allowing to intercept messages and separately control
// start and stop on both directions of the duplex
func NewPipe(source, dest StratumReadWriter, sourceInterceptor, destInterceptor Interceptor, log gi.ILogger) *Pipe {
	pipe := &Pipe{
		source:            source,
		dest:              dest,
		sourceInterceptor: sourceInterceptor,
		destInterceptor:   destInterceptor,
		log:               log,
	}

	sourceToDestTask := lib.NewTaskFunc(pipe.sourceToDest)
	destToSourceTask := lib.NewTaskFunc(pipe.destToSource)

	pipe.sourceToDestTask = sourceToDestTask
	pipe.destToSourceTask = destToSourceTask

	return pipe
}

func (p *Pipe) Run(ctx context.Context) error {
	var err error

	select {
	case <-p.sourceToDestTask.Done():
		err = p.sourceToDestTask.Err()
		<-p.destToSourceTask.Stop()
	case <-p.destToSourceTask.Done():
		err = p.destToSourceTask.Err()
		<-p.sourceToDestTask.Stop()
	case <-ctx.Done():
		<-p.sourceToDestTask.Stop()
		<-p.destToSourceTask.Stop()
		err = ctx.Err()
	}

	return err
}

func (p *Pipe) destToSource(ctx context.Context) error {
	err := pipe(ctx, p.GetDest, p.GetSource, p.destInterceptor)
	if err != nil {
		err := fmt.Errorf("dest to source pipe err: %w", err)
		p.log.Debug(err)
		return err
	}
	return nil
}

func (p *Pipe) sourceToDest(ctx context.Context) error {
	err := pipe(ctx, p.GetSource, p.GetDest, p.sourceInterceptor)
	if err != nil {
		err := fmt.Errorf("source to dest pipe err: %w", err)
		p.log.Debug(err)
		return err
	}
	return nil
}

// pipe reads from from() and writes to to(), intercepting messages with interceptor
// implemented late binding to enable replacing the source and dest at runtime of the function
// TODO: consider stopping and then recreating pipe when source or dest changes
func pipe(ctx context.Context, from func() StratumReadWriter, to func() StratumReadWriter, interceptor Interceptor) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		msg, err := from().Read(ctx)
		if err != nil {
			return fmt.Errorf("source read err: %w", err)
		}

		msg, err = interceptor(msg)
		if err != nil {
			return fmt.Errorf("interceptor err: %w", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if msg == nil {
			continue
		}

		err = to().Write(ctx, msg)
		if err != nil {
			return fmt.Errorf("dest write err: %w %s", err, string(msg.Serialize()))
		}
	}
}

func (p *Pipe) GetDest() StratumReadWriter {
	return p.dest
}

func (p *Pipe) SetDest(dest StratumReadWriter) {
	p.dest = dest
}

func (p *Pipe) GetSource() StratumReadWriter {
	return p.source
}

func (p *Pipe) SetSource(source StratumReadWriter) {
	p.source = source
}

func (p *Pipe) StartSourceToDest(ctx context.Context) {
	p.sourceToDestTask.Start(ctx)
}
func (p *Pipe) StartDestToSource(ctx context.Context) {
	p.destToSourceTask.Start(ctx)
}
func (p *Pipe) StopSourceToDest() <-chan struct{} {
	return p.sourceToDestTask.Stop()
}
func (p *Pipe) StopDestToSource() <-chan struct{} {
	return p.destToSourceTask.Stop()
}
