package gostun

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

type Client struct {
	conn        Connection
	TimeoutRate time.Duration
	wg          sync.WaitGroup
	close       chan struct{}
	agent       messageClient
	rw          sync.RWMutex
	clientclose bool
}

type messageClient interface {
	ProcessHandle(*Message) error
	TimeOutHandle(time.Time) error
	TransactionHandle([TransactionIDSize]byte, Handler, time.Time) error
}

type Connection interface {
	io.Reader
	io.Writer
	io.Closer
}

const defaultTimeoutRate = time.Millisecond * 100

func Dial(network, addr string) (*Client, error) {
	conn, err := net.Dial(network, addr)
	if err != nil {
		return nil, err
	}
	return NewClient(conn)
}

func NewClient(conn net.Conn) (*Client, error) {
	c := &Client{
		conn:        conn,
		TimeoutRate: defaultTimeoutRate,
	}

	if c.agent == nil {
		c.agent = NewAgent()
	}

	c.wg.Add(2)
	go c.readUntil()
	go c.collectUntil()

	return c, nil
}

func (c *Client) readUntil() {
	defer c.wg.Done()

	m := new(Message)
	m.Raw = make([]byte, 1024)
	for {
		select {
		case <-c.close:
			return
		default:
		}
		_, err := m.ReadConn(c.conn) // read and decode message
		if err == nil {
			if processErr := c.agent.ProcessHandle(m); processErr != nil {
				return
			}
		}
	}
}

func (c *Client) collectUntil() {
	t := time.NewTicker(c.TimeoutRate)
	defer c.wg.Done()
	for {
		select {
		case <-c.close:
			t.Stop()
			return
		case trate := <-t.C:
			err := c.agent.TimeOutHandle(trate)
			if err == nil || err == ErrAgent {
				return
			}
			panic(err)
		}
	}
}

// process of transaction in message
type Agent struct {
	transactions map[transactionID]TransactionAgent
	mux          sync.Mutex
	nonHandler   Handler // non-registered transactions
	closed       bool
}

type transactionID [TransactionIDSize]byte //12byte, 96bit

// transaction in progress
type TransactionAgent struct {
	ID      transactionID
	Timeout time.Time
	handler Handler // if transaction is succeed will be called
}

type AgentHandle struct {
	handler Handler
}

// reference http.HandlerFunc same work
type Handler interface {
	HandleEvent(e EventObject)
}

type HandleFunc func(e EventObject)

func (f HandleFunc) HandleEvent(e EventObject) {
	f(e)
}

type EventObject struct {
	Msg *Message
	err error
}

func NewAgent() *Agent {
	h := AgentHandle{}
	a := &Agent{
		transactions: make(map[transactionID]TransactionAgent),
		nonHandler:   h.handler,
	}
	return a
}

func (a *Agent) ProcessHandle(m *Message) error {
	e := EventObject{
		Msg: m,
	}
	a.mux.Lock() // protect transaction
	tr, ok := a.transactions[m.TransactionID]
	delete(a.transactions, m.TransactionID) //delete maps entry

	if ok {
		tr.handler.HandleEvent(e) // HandleEvent cast the e to hander type
	} else if a.nonHandler != nil {
		a.nonHandler.HandleEvent(e) // the transaction is not registered
	}
	return nil
}

/*
すべてのハンドラがTransactionTimeOutErrを処理するまで、
指定された時刻より前にデッドラインを持つすべてのトランザクションをblockする。
エージェントが既に閉じられている場合、ErrAgentを返す
*/

var (
	ErrAgent              = errors.New("agent closed")
	TransactionTimeOutErr = errors.New("transaction is timed out")
)

/*
The value for RTO SHOULD be cached by a client after the completion
of the transaction, and used as the starting value for RTO for the
next transaction to the same server (based on equality of IP
address).
*/

func (a *Agent) TimeOutHandle(trate time.Time) error {
	call := make([]Handler, 0, 100)
	remove := make([]transactionID, 0, 100)
	a.mux.Lock()

	if a.closed {
		a.mux.Unlock()
		return ErrAgent
	}

	for i, tr := range a.transactions {
		if tr.Timeout.Before(trate) {
			call = append(call, tr.handler)
			remove = append(remove, i)
		}
	}

	// no registered transactions
	for _, id := range remove {
		delete(a.transactions, id)
	}

	a.mux.Unlock()
	e := EventObject{
		err: TransactionTimeOutErr,
	}
	// return transactions
	for _, h := range call {
		h.HandleEvent(e)
	}

	return nil
}
