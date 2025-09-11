package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Dreamacro/clash/common/queue"
	"github.com/Dreamacro/clash/component/dialer"
	C "github.com/Dreamacro/clash/constant"
	"github.com/Dreamacro/clash/log"

	"go.uber.org/atomic"
)

type Proxy struct {
	C.ProxyAdapter
	history *queue.Queue
	alive   *atomic.Bool
}

// Alive implements C.Proxy
func (p *Proxy) Alive() bool {
	return p.alive.Load()
}

// Dial implements C.Proxy
func (p *Proxy) Dial(metadata *C.Metadata) (C.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), C.DefaultTCPTimeout)
	defer cancel()
	return p.DialContext(ctx, metadata)
}

// DialContext implements C.ProxyAdapter
func (p *Proxy) DialContext(ctx context.Context, metadata *C.Metadata, opts ...dialer.Option) (C.Conn, error) {
	conn, err := p.ProxyAdapter.DialContext(ctx, metadata, opts...)
	p.alive.Store(err == nil)
	return conn, err
}

// DialUDP implements C.ProxyAdapter
func (p *Proxy) DialUDP(metadata *C.Metadata) (C.PacketConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), C.DefaultUDPTimeout)
	defer cancel()
	return p.ListenPacketContext(ctx, metadata)
}

// ListenPacketContext implements C.ProxyAdapter
func (p *Proxy) ListenPacketContext(ctx context.Context, metadata *C.Metadata, opts ...dialer.Option) (C.PacketConn, error) {
	pc, err := p.ProxyAdapter.ListenPacketContext(ctx, metadata, opts...)
	p.alive.Store(err == nil)
	return pc, err
}

// DelayHistory implements C.Proxy
func (p *Proxy) DelayHistory() []C.DelayHistory {
	queue := p.history.Copy()
	histories := []C.DelayHistory{}
	for _, item := range queue {
		histories = append(histories, item.(C.DelayHistory))
	}
	return histories
}

// LastDelay return last history record. if proxy is not alive, return the max value of uint16.
// implements C.Proxy
func (p *Proxy) LastDelay() (delay uint16) {
	var max uint16 = 0xffff
	if !p.alive.Load() {
		return max
	}

	last := p.history.Last()
	if last == nil {
		return max
	}
	history := last.(C.DelayHistory)
	if history.Delay == 0 {
		return max
	}
	return history.Delay
}

// MarshalJSON implements C.ProxyAdapter
func (p *Proxy) MarshalJSON() ([]byte, error) {
	inner, err := p.ProxyAdapter.MarshalJSON()
	if err != nil {
		return inner, err
	}

	mapping := map[string]any{}
	json.Unmarshal(inner, &mapping)
	mapping["history"] = p.DelayHistory()
	mapping["alive"] = p.Alive()
	mapping["name"] = p.Name()
	mapping["udp"] = p.SupportUDP()
	return json.Marshal(mapping)
}

// URLTest get the delay for the specified URL
// implements C.Proxy
func (p *Proxy) URLTest(ctx context.Context, url string) (delay, meanDelay uint16, err error) {
	defer func() {
		p.alive.Store(err == nil)
		record := C.DelayHistory{Time: time.Now()}
		if err == nil {
			record.Delay = delay
			record.MeanDelay = meanDelay
		}
		p.history.Put(record)
		if p.history.Len() > 10 {
			p.history.Pop()
		}
	}()

	addr, err := urlToMetadata(url)
	if err != nil {
		return
	}

	if addr.NetWork == C.UDP {
		return p.udpTest(ctx, &addr)
	}
	return p.tcpTest(ctx, url, &addr)
}

func (p *Proxy) tcpTest(ctx context.Context, url string, addr *C.Metadata) (delay, meanDelay uint16, err error) {
	start := time.Now()
	instance, err := p.DialContext(ctx, addr)
	if err != nil {
		return
	}
	defer instance.Close()

	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return
	}
	req = req.WithContext(ctx)

	transport := &http.Transport{
		Dial: func(string, string) (net.Conn, error) {
			return instance, nil
		},
		// from http.DefaultTransport
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	client := http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer client.CloseIdleConnections()

	resp, err := client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
	delay = uint16(time.Since(start) / time.Millisecond)

	resp, err = client.Do(req)
	if err != nil {
		// ignore error because some server will hijack the connection and close immediately
		return delay, 0, nil
	}
	resp.Body.Close()
	meanDelay = uint16(time.Since(start) / time.Millisecond / 2)
	return
}

func (p *Proxy) udpTest(ctx context.Context, addr *C.Metadata) (delay, meanDelay uint16, err error) {
	start := time.Now()

	instance, err := p.ListenPacketContext(ctx, addr)
	if err != nil {
		log.Debugln("udpTest %s %v", addr.RemoteAddress(), err)
		return
	}
	defer instance.Close()

	_, err = instance.WriteTo([]byte("PING"), addr.UDPAddr())
	if err != nil {
		return
	}

	instance.SetReadDeadline(time.Now().Add(time.Second * 5))
	_, _, err = instance.ReadFrom(make([]byte, 32))
	if err != nil {
		log.Debugln("udpTest %s %v", addr.RemoteAddress(), err)
		return
	}

	delay = uint16(time.Since(start) / time.Millisecond)

	_, err = instance.WriteTo([]byte("PING"), addr.UDPAddr())
	if err != nil {
		return
	}

	instance.SetReadDeadline(time.Now().Add(time.Second * 5))
	_, _, err = instance.ReadFrom(make([]byte, 32))
	if err != nil {
		log.Debugln("udpTest delay %s %d", addr.RemoteAddress(), delay)
		return delay, 0, nil
	}

	meanDelay = uint16(time.Since(start) / time.Millisecond / 2)
	log.Debugln("udpTest delay %s %d %d", addr.RemoteAddress(), delay, meanDelay)
	return
}

func NewProxy(adapter C.ProxyAdapter) *Proxy {
	return &Proxy{adapter, queue.New(10), atomic.NewBool(true)}
}

func urlToMetadata(rawURL string) (addr C.Metadata, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return
	}

	netWork := C.TCP
	if strings.ToLower(u.Scheme) == "udp" {
		netWork = C.UDP
	}

	port := u.Port()
	if port == "" {
		switch u.Scheme {

		case "https":
			port = "443"
		case "http":
			port = "80"
		default:
			err = fmt.Errorf("%s scheme not Support", rawURL)
			return
		}
	}

	p, _ := strconv.ParseUint(port, 10, 16)

	addr = C.Metadata{
		NetWork: netWork,
		Host:    u.Hostname(),
		DstIP:   net.ParseIP(u.Hostname()),
		DstPort: C.Port(p),
	}
	return
}
