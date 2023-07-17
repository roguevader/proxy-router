package proxy

import (
	"context"
	"fmt"
	"net/url"
	"sync"

	gi "gitlab.com/TitanInd/proxy/proxy-router-v3/internal/interfaces"
	"gitlab.com/TitanInd/proxy/proxy-router-v3/internal/lib"
	i "gitlab.com/TitanInd/proxy/proxy-router-v3/internal/resources/hashrate/proxy/interfaces"
	sm "gitlab.com/TitanInd/proxy/proxy-router-v3/internal/resources/hashrate/proxy/stratumv1_message"
)

// ConnDest is a destination connection, a wrapper around StratumConnection,
// with destination specific state variables
type ConnDest struct {
	// config
	workerName string
	destUrl    *url.URL

	// state
	diff           float64
	hr             gi.Hashrate
	resultHandlers sync.Map // map[string]func(*stratumv1_message.MiningResult)

	extraNonce     string
	extraNonceSize int

	versionRolling     bool
	versionRollingMask string

	notifyMsgs *lib.BoundStackMap[*sm.MiningNotify]

	// deps
	conn *StratumConnection
	log  gi.ILogger
}

const (
	NOTIFY_MSGS_CACHE_SIZE = 30
)

func NewDestConn(conn *StratumConnection, url *url.URL, log gi.ILogger) *ConnDest {
	return &ConnDest{
		workerName: url.User.Username(),
		destUrl:    url,
		conn:       conn,
		log:        log,
		notifyMsgs: lib.NewBoundStackMap[*sm.MiningNotify](NOTIFY_MSGS_CACHE_SIZE),
	}
}

func ConnectDest(ctx context.Context, destURL *url.URL, log gi.ILogger) (*ConnDest, error) {
	destLog := log.Named(fmt.Sprintf("[DST] %s@%s", destURL.User.Username(), destURL.Host))
	conn, err := Connect(destURL, CONNECTION_TIMEOUT, destLog)
	if err != nil {
		return nil, err
	}

	return NewDestConn(conn, destURL, destLog), nil
}

// AutoRead reads incoming jobs from the destination connection and
// caches them so dest will not close the connection
func (c *ConnDest) AutoRead(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		_, err := c.Read(ctx)
		if err != nil {
			return err
		}
	}
}

func (c *ConnDest) GetID() string {
	return c.conn.GetID()
}

func (c *ConnDest) Read(ctx context.Context) (i.MiningMessageGeneric, error) {
	msg, err := c.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	return c.readInterceptor(msg)
}

func (c *ConnDest) Write(ctx context.Context, msg i.MiningMessageGeneric) error {
	return c.conn.Write(ctx, msg)
}

func (c *ConnDest) GetExtraNonce() (extraNonce string, extraNonceSize int) {
	return c.extraNonce, c.extraNonceSize
}

func (c *ConnDest) SetExtraNonce(extraNonce string, extraNonceSize int) {
	c.extraNonce, c.extraNonceSize = extraNonce, extraNonceSize
}

func (c *ConnDest) GetVersionRolling() (versionRolling bool, versionRollingMask string) {
	return c.versionRolling, c.versionRollingMask
}

func (c *ConnDest) SetVersionRolling(versionRolling bool, versionRollingMask string) {
	c.versionRolling, c.versionRollingMask = versionRolling, versionRollingMask
}

// TODO: guard with mutex
func (c *ConnDest) GetDiff() float64 {
	return c.diff
}

func (c *ConnDest) GetHR() gi.Hashrate {
	return c.hr
}

func (c *ConnDest) GetNotifyMsgJob(jobID string) (*sm.MiningNotify, bool) {
	return c.notifyMsgs.Get(jobID)
}

func (c *ConnDest) GetWorkerName() string {
	return c.workerName
}

func (c *ConnDest) SetWorkerName(workerName string) {
	c.workerName = workerName
}

func (c *ConnDest) readInterceptor(msg i.MiningMessageGeneric) (resMsg i.MiningMessageGeneric, err error) {
	switch typed := msg.(type) {
	case *sm.MiningNotify:
		c.notifyMsgs.Push(typed.GetJobID(), typed)
	case *sm.MiningSetDifficulty:
		c.diff = typed.GetDifficulty()
	case *sm.MiningSetExtranonce:
		c.extraNonce, c.extraNonceSize = typed.GetExtranonce()
	// TODO: handle set_version_mask, multiversion
	case *sm.MiningResult:
		handler, ok := c.resultHandlers.LoadAndDelete(typed.GetID())
		if ok {
			resMsg, err := handler.(ResultHandler)(typed)
			if err != nil {
				return nil, err
			}
			return resMsg, nil
		}
	}
	return msg, nil
}

// TODO: consider moving to proxy.go
// onceResult registers single time handler for the destination response with particular message ID,
// sets default timeout and does a cleanup when it expires. Returns error on result timeout
func (s *ConnDest) onceResult(ctx context.Context, msgID int, handler ResultHandler) <-chan error {
	errCh := make(chan error, 1)

	ctx, cancel := context.WithTimeout(ctx, RESPONSE_TIMEOUT)
	didRun := false

	s.resultHandlers.Store(msgID, func(a *sm.MiningResult) (msg i.MiningMessageWithID, err error) {
		didRun = true
		defer cancel()
		defer close(errCh)
		return handler(a)
	})

	go func() {
		<-ctx.Done()
		s.resultHandlers.Delete(msgID)
		if !didRun {
			errCh <- fmt.Errorf("dest response timeout (%s)", RESPONSE_TIMEOUT)
		}
	}()

	return errCh
}

// WriteAwaitRes writes message to the destination connection and awaits for the response, but does not proxy it to source
func (s *ConnDest) WriteAwaitRes(ctx context.Context, msg i.MiningMessageWithID) (resMsg i.MiningMessageWithID, err error) {
	errCh := make(chan error, 1)
	resCh := make(chan i.MiningMessageWithID, 1)
	msgID := msg.GetID()

	ctx, cancel := context.WithTimeout(ctx, RESPONSE_TIMEOUT)
	didRun := false

	s.resultHandlers.Store(msgID, func(a *sm.MiningResult) (msg i.MiningMessageWithID, err error) {
		didRun = true
		defer cancel()
		defer close(errCh)
		resCh <- a
		return nil, nil
	})

	err = s.Write(ctx, msg)
	if err != nil {
		s.resultHandlers.Delete(msgID)
		return nil, err
	}

	go func() {
		<-ctx.Done()
		s.resultHandlers.Delete(msgID)
		if !didRun {
			errCh <- fmt.Errorf("dest response timeout (%s)", RESPONSE_TIMEOUT)
		}
	}()

	return <-resCh, <-errCh
}
