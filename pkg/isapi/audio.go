package isapi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/tcp"
)

// audioData switches from HTTP headers to unframed G.711 bytes. Own the socket
// instead of borrowing one from http.Transport's keep-alive pool/read loop.
func (c *Client) openAudio(ctx context.Context) (net.Conn, *bufio.Reader, error) {
	u, err := url.Parse(c.url + "/ISAPI/System/TwoWayAudio/channels/" + c.channel + "/audioData")
	if err != nil {
		return nil, nil, err
	}
	user := u.User
	u.User = nil // Hikvision requires a Digest challenge, not preemptive Basic auth.
	address := u.Host
	if u.Port() == "" {
		address = net.JoinHostPort(u.Hostname(), "80")
	}
	authorization := ""
	for attempt := 0; attempt < 2; attempt++ {
		conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", address)
		if err != nil {
			return nil, nil, err
		}
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		req, _ := http.NewRequest("PUT", u.String(), nil)
		req.Header.Set("Content-Type", "application/octet-stream")
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		reader := bufio.NewReader(conn)
		if err = req.Write(conn); err == nil {
			var res *http.Response
			res, err = http.ReadResponse(reader, req)
			if err == nil {
				if res.StatusCode == http.StatusOK {
					conn.SetDeadline(time.Time{})
					// Cancellation remains registered until the session ends. The owning Open
					// cancels ctx on every return path, releasing this callback and the socket.
					return conn, reader, nil
				}
				if res.StatusCode == http.StatusUnauthorized && attempt == 0 && user != nil {
					authorization, err = audioDigest(user, res.Header.Get("WWW-Authenticate"), req.Method, req.URL.RequestURI())
				} else {
					err = fmt.Errorf("isapi: audio HTTP %d", res.StatusCode)
				}
			}
		}
		stop()
		conn.Close()
		if err != nil {
			return nil, nil, err
		}
	}
	return nil, nil, errors.New("isapi: audio authentication failed")
}

func audioDigest(user *url.Userinfo, challenge, method, uri string) (string, error) {
	if !strings.HasPrefix(challenge, "Digest ") {
		return "", errors.New("isapi: expected Digest authentication")
	}
	realm := tcp.Between(challenge, `realm="`, `"`)
	nonce := tcp.Between(challenge, `nonce="`, `"`)
	if realm == "" || nonce == "" {
		return "", errors.New("isapi: incomplete Digest challenge")
	}
	qop := tcp.Between(challenge, `qop="`, `"`)
	algorithm := ""
	if _, rest, ok := strings.Cut(challenge, "algorithm="); ok {
		algorithm = strings.SplitN(rest, ",", 2)[0]
	}
	if algorithm != "" && strings.Trim(algorithm, "\" ") != "MD5" {
		return "", errors.New("isapi: unsupported Digest algorithm")
	}
	password, _ := user.Password()
	ha1 := tcp.HexMD5(user.Username(), realm, password)
	ha2 := tcp.HexMD5(method, uri)
	header := fmt.Sprintf(`Digest username=%q, realm=%q, nonce=%q, uri=%q`, user.Username(), realm, nonce, uri)
	if qop == "" {
		header += fmt.Sprintf(`, response="%s"`, tcp.HexMD5(ha1, nonce, ha2))
	} else {
		hasAuth := false
		for _, part := range strings.Split(qop, ",") {
			if strings.TrimSpace(part) == "auth" {
				hasAuth = true
			}
		}
		if !hasAuth {
			return "", errors.New("isapi: unsupported Digest qop")
		}
		cnonce := core.RandString(32, 64)
		header += fmt.Sprintf(`, qop=auth, nc=00000001, cnonce="%s", response="%s"`, cnonce, tcp.HexMD5(ha1, nonce, "00000001", cnonce, "auth", ha2))
	}
	if opaque := tcp.Between(challenge, `opaque="`, `"`); opaque != "" {
		header += fmt.Sprintf(`, opaque=%q`, opaque)
	}
	return header, nil
}
