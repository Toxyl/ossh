package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

type SyncClient struct {
	Host string
	Port int
	conn net.Conn
}

func (sc *SyncClient) connect() {
	c, err := net.Dial("tcp", sc.ID())
	if err != nil {
		LogSyncClient.Error("Failed to connect: %s", colorError(err))
		return
	}
	LogSyncClient.Debug("%s: connect", colorHost(sc.ID()))
	sc.conn = c
}

func (sc *SyncClient) write(msg string) {
	if sc.conn != nil {
		_ = sc.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		msg = strings.TrimSpace(msg)
		LogSyncClient.Debug("%s: write: %s", colorHost(sc.ID()), colorReason(msg))
		msg = EncodeGzBase64String(msg)
		LogSyncClient.Debug("%s: write: %s", colorHost(sc.ID()), colorHighlight(msg))
		fmt.Fprintf(sc.conn, msg+"\n")
	}
}

func (sc *SyncClient) exit() {
	LogSyncClient.Debug("%s: exit", colorHost(sc.ID()))
	sc.write("exit")
}

func (sc *SyncClient) Exec(command string) (string, error) {
	sc.connect()
	if sc.conn == nil {
		return "", errors.New("not connected")
	}
	sc.write(command)
	defer sc.exit()

	_ = sc.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	reader := bufio.NewReader(sc.conn)
	resp, err := reader.ReadString('\n')

	if err != nil && err != io.EOF {
		LogSyncClient.Error("\"%s\", failed to read response: %s", colorHighlight(command), colorError(err))
		return "", err
	}
	resp = strings.TrimSpace(resp)
	LogSyncClient.Debug("%s: read: %s", colorHost(sc.ID()), colorReason(resp))
	if resp == "" {
		return "", nil
	}
	resp, err = DecodeGzBase64String(resp)
	LogSyncClient.Debug("%s: read: %s", colorHost(sc.ID()), colorHighlight(resp))
	if err != nil {
		LogSyncClient.Debug("%s: Decoding %s failed: %s", colorHost(sc.ID()), colorHighlight(command), colorError(err))
		LogSyncClient.Debug("%s: Response was: %s", colorHost(sc.ID()), colorHighlight(resp))
	}

	return resp, nil
}

func (sc *SyncClient) ID() string {
	return fmt.Sprintf("%s:%d", sc.Host, sc.Port)
}

func (sc *SyncClient) AddHost(host string) {
	_, _ = sc.Exec(fmt.Sprintf("ADD-HOST %s", host))
}

func (sc *SyncClient) AddUser(user string) {
	_, _ = sc.Exec(fmt.Sprintf("ADD-USER %s", user))
}

func (sc *SyncClient) AddPassword(password string) {
	_, _ = sc.Exec(fmt.Sprintf("ADD-PASSWORD %s", password))
}

func (sc *SyncClient) AddFingerprint(fingerprint string) {
	_, _ = sc.Exec(fmt.Sprintf("ADD-FINGERPRINT %s", fingerprint))
}

func (sc *SyncClient) SyncData(cmd string, fnGet func() []string, fnAddRemote func(data string)) {
	LogSyncClient.Debug("%s: syncing %s", colorHost(sc.ID()), colorHighlight(cmd))
	res, err := sc.Exec(cmd)
	if err != nil {
		// chances are that the node refused the connection because it's busy with syncing itself
		LogSyncClient.Debug("Failed to get %s: %s", colorHighlight(cmd), colorError(err))
		return
	}

	fnAddRemote(strings.Join(StringSliceDifference(fnGet(), ExplodeLines(res)), " "))
}

func NewSyncClient(host string, port int) *SyncClient {
	sc := &SyncClient{
		Host: host,
		Port: port,
	}
	return sc
}
