package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/exp/maps"
)

const InvalidCommand = "Command not recognized"
const EmptyCommandResponse = ""
const ExitCommand = "exit"

var SyncServerCommands = NewSyncCommands()

type SyncServerConnection struct {
	conn      net.Conn
	Host      string
	Port      int
	CreatedAt time.Time
	lock      *sync.Mutex
}

func (ssc *SyncServerConnection) LogID() string {
	if ssc.conn != nil {
		lhost, lport := SplitHostPortFromAddr(ssc.conn.LocalAddr())
		rhost, rport := SplitHostPortFromAddr(ssc.conn.RemoteAddr())
		return fmt.Sprintf("%s -> %s", colorConnID("", lhost, lport), colorConnID("", rhost, rport))
	}
	return colorConnID("", ssc.Host, ssc.Port)
}

func (ssc *SyncServerConnection) close() {
	ssc.lock.Lock()
	defer ssc.lock.Unlock()
	if ssc.conn != nil {
		ssc.conn.Close()
		ssc.conn = nil
	}
}

func (ssc *SyncServerConnection) write(msg string) {
	msg = strings.TrimSpace(msg)
	if msg == "" || ssc.conn == nil {
		return
	}
	_, _ = ssc.conn.Write([]byte(EncodeGzBase64String(msg)))
}

func (ssc *SyncServerConnection) process(cmd string) error {
	str := strings.Split(cmd, " ")

	if len(str) <= 0 {
		ssc.write(InvalidCommand)
		return errors.New("invalid command")
	}

	res, err := SyncServerCommands.Run(ssc, str[0], str[1:])
	if err != nil {
		LogSyncServer.Error("%s: Error running sync command %s: %s", ssc.LogID(), colorHighlight(str[0]), colorError(err))
		ssc.write(EmptyCommandResponse)
		return err
	}
	ssc.write(res)
	return nil
}

func (ssc *SyncServerConnection) handleConnection() error {
	ssc.lock.Lock()
	defer ssc.lock.Unlock()

	if ssc.conn == nil {
		return errors.New("not connected")
	}

	s := bufio.NewScanner(ssc.conn)
	for s.Scan() {
		data := s.Text()
		data, err := DecodeGzBase64String(data)
		if err != nil {
			return fmt.Errorf("could not decode input: %s", colorError(err))
		}

		if data == EmptyCommandResponse || data == ExitCommand {
			ssc.write(data)
			return nil
		}

		err = ssc.process(data)
		if err != nil {
			return fmt.Errorf("failed to process: %s", colorError(err))
		}
	}
	return nil
}

type SyncServerConnections struct {
	conns map[string]*SyncServerConnection
	lock  *sync.Mutex
}

func (sscs *SyncServerConnections) Length() int {
	sscs.lock.Lock()
	defer sscs.lock.Unlock()
	return len(sscs.conns)
}

func (sscs *SyncServerConnections) Hosts() []string {
	sscs.lock.Lock()
	defer sscs.lock.Unlock()
	keys := maps.Keys(sscs.conns)
	for i, k := range keys {
		keys[i] = ExtractHost(k)
	}

	return keys
}

func (sscs *SyncServerConnections) Create(conn net.Conn, host string, port int) *SyncServerConnection {
	sscs.lock.Lock()
	defer sscs.lock.Unlock()
	sid := fmt.Sprintf("%s:%d", host, port)
	create := false
	if _, ok := sscs.conns[sid]; !ok {
		create = true
	} else if sscs.conns[sid].conn == nil {
		create = true
	} else if CLEANUP_SYNC_MIN_AGE < time.Since(sscs.conns[sid].CreatedAt) {
		sscs.conns[sid].close()
		create = true
	}

	if create {
		sscs.conns[sid] = &SyncServerConnection{
			conn:      conn,
			Host:      host,
			Port:      port,
			CreatedAt: time.Now(),
			lock:      &sync.Mutex{},
		}
	}
	return sscs.conns[sid]
}

func (sscs *SyncServerConnections) Remove(host string, port int) error {
	sscs.lock.Lock()
	defer sscs.lock.Unlock()
	sid := fmt.Sprintf("%s:%d", host, port)
	if _, ok := sscs.conns[sid]; ok {
		sscs.conns[sid].close()
		sscs.conns[sid] = nil
		delete(sscs.conns, sid)
		return nil
	}
	return fmt.Errorf("connection %s was not found", colorConnID("", host, port))
}

func (sscs *SyncServerConnections) CloseAll() {
	sscs.lock.Lock()
	defer sscs.lock.Unlock()

	for _, v := range sscs.conns {
		v.close()
	}
}

func (sscs *SyncServerConnections) CleanUp() []string {
	sscs.lock.Lock()
	defer sscs.lock.Unlock()

	removed := []string{}

	for _, v := range sscs.conns {
		if CLEANUP_SYNC_MIN_AGE < time.Since(v.CreatedAt) {
			v.close()
			removed = append(removed, v.Host)
		}
	}

	return removed
}

func NewSyncServerConnections() *SyncServerConnections {
	return &SyncServerConnections{
		conns: map[string]*SyncServerConnection{},
		lock:  &sync.Mutex{},
	}
}

type SyncServer struct {
	listener net.Listener
	conns    *SyncServerConnections
	nodes    *SyncNodes
}

func (ss *SyncServer) close() {
	ss.conns.CloseAll()
}

func (ss *SyncServer) HasNode(host string) bool {
	return ss.nodes.Has(host)
}

func (ss *SyncServer) GetNode(host string) (*SyncNode, error) {
	return ss.nodes.Get(host)
}

func (ss *SyncServer) GetClient(host string, port int) (*SyncClient, error) {
	cid := fmt.Sprintf("%s:%d", host, port)
	if ss.nodes.HasClient(cid) {
		return ss.nodes.GetClient(cid)
	}
	return nil, errors.New("sync client not found")
}

func (ss *SyncServer) AddClient(host string, port int) {
	if host == Conf.SyncServer.Host && port == int(Conf.SyncServer.Port) {
		return // so we don't accidentally add ourselves
	}
	ss.nodes.AddClient(NewSyncClient(host, port))
}

func (ss *SyncServer) RemoveClient(host string, port int) {
	ss.nodes.RemoveClient(fmt.Sprintf("%s:%d", host, port))
}

func (ss *SyncServer) Broadcast(msg string) map[string]string {
	return ss.nodes.ExecBroadcast(msg)
}

func (ss *SyncServer) Exec(msg string) string {
	return ss.nodes.Exec(msg)
}

func (ss *SyncServer) GetOutOfSyncNodes(fingerprint string) map[string]string {
	res := ss.Broadcast(fmt.Sprintf("SYNC %s", fingerprint))
	return res
}

func (ss *SyncServer) SyncWorker() {
	RandomSleep(30, 60, time.Second)
	for {
		fp := SrvOSSH.Loot.Fingerprint()
		fp = strings.TrimSpace(fp)

		for k, v := range ss.GetOutOfSyncNodes(fp) {
			v = strings.TrimSpace(v)
			if v == "" {
				continue // node is already in sync
			}
			sections := strings.Split(v, ",")
			host, port := SplitHostPort(k)
			LogSyncServer.Debug("%s: %s are outdated", colorConnID("", host, port), colorHighlight(v))
			client, err := ss.GetClient(host, port)
			if err != nil {
				LogSyncServer.Error("%s: Failed to get client: %s", colorConnID("", host, port), colorError(err))
				continue
			}

			for _, section := range sections {
				LogSyncServer.Debug("%s: Sending %s", colorConnID("", host, port), colorHighlight(section))
				switch section {
				case "hosts":
					client.SyncData("HOSTS", SrvOSSH.Loot.GetHosts, client.AddHosts)
				case "users":
					client.SyncData("USERS", SrvOSSH.Loot.GetUsers, client.AddUsers)
				case "passwords":
					client.SyncData("PASSWORDS", SrvOSSH.Loot.GetPasswords, client.AddPasswords)
				case "payloads":
					client.SyncData("PAYLOADS", SrvOSSH.Loot.GetPayloads, client.AddPayload)
				}
			}
		}
		LogSyncServer.Debug("Sync complete!")

		time.Sleep(time.Duration(Conf.Sync.Interval) * time.Minute)
	}
}

func (ss *SyncServer) UpdateClients() {
	// remove existing clients
	for _, c := range ss.nodes.clients {
		c.conn.Close()
		ip := c.Host
		port := c.Port
		index := -1
		for i, v := range Conf.IPWhitelist {
			if v == ip {
				index = i
				break
			}
		}
		if index >= 0 {
			Conf.IPWhitelist = append(Conf.IPWhitelist[:index], Conf.IPWhitelist[index+1:]...)
		}

		ss.RemoveClient(ip, port)
	}

	// add clients
	for _, node := range Conf.Sync.Nodes {
		if node.Host != Conf.SyncServer.Host || node.Port != int(Conf.SyncServer.Port) {
			Conf.IPWhitelist = append(Conf.IPWhitelist, node.Host)
			LogSyncServer.Debug("%s: Client added", node.LogID())
			ss.nodes.AddClient(NewSyncClient(node.Host, node.Port))
		}
	}
}

func (ss *SyncServer) ConnectionHandler(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			LogSyncServer.Error("Accept failed: %s", colorError(err))
			conn.Close()
			continue
		}

		host, port := SplitHostPortFromAddr(conn.RemoteAddr())
		ssc := ss.conns.Create(conn, host, port)
		lid := ssc.LogID()

		if !ss.nodes.IsAllowedHost(host) {
			LogSyncServer.NotOK("%s: Not a sync node, returning bullshit.", lid)
			ssc.write(GenerateGarbageString(1000))
			_ = ss.conns.Remove(host, port)
			continue
		}

		go func(host string, port int) {
			err := ssc.handleConnection()
			if err != nil {
				LogSyncServer.Error("%s: %s", lid, err)
			}

			err = ss.conns.Remove(host, port)
			if err != nil {
				LogSyncServer.Error("%s: Could not remove goroutine: %s", lid, err)
			}
		}(host, port)
	}
}

func (ss *SyncServer) CleanUpWorker() {
	RandomSleep(30, 60, time.Second)
	for {
		time.Sleep(INTERVAL_SYNC_CLEANUP)
		removed := ss.conns.CleanUp()
		lr := len(removed)
		l := ss.conns.Length()
		hs := colorHosts(ss.conns.Hosts())
		hsr := colorHosts(removed)
		if lr > 0 {
			if l == 0 {
				LogSyncServer.Info("Cleanup worker: Removed %s, none left", hsr)
				continue
			}
			LogSyncServer.Info("Cleanup worker: Removed %s, still open: %s", hsr, hs)
		}
	}
}

func (ss *SyncServer) Start() {
	ss.UpdateClients()
	srv := fmt.Sprintf("%s:%d", Conf.SyncServer.Host, Conf.SyncServer.Port)
	LogSyncServer.Default("Starting sync server on %s...", colorWrap("tcp://"+srv, colorBrightYellow))
	listener, err := net.Listen("tcp", srv)
	if err != nil {
		panic(err)
	}
	go ss.ConnectionHandler(listener)
	go ss.SyncWorker()
	go ss.CleanUpWorker()
}

func NewSyncServer() *SyncServer {
	ss := &SyncServer{
		listener: nil,
		nodes:    NewSyncNodes(),
		conns:    NewSyncServerConnections(),
	}

	return ss
}
