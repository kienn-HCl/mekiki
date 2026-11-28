package digest

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// 記事ページのリンクはフィードの配信元が決めるので、mekiki 自身や LAN・クラウドのメタデータなど
// 内部のアドレスには接続しない（悪意あるフィードに内部の情報を取らせないため）。
// 接続のたびに名前解決後のアドレスを確かめるので、リダイレクトや DNS の差し替えでも抜けられない。
// プロキシは使わず直接つなぐ。

var errInternalAddr = errors.New("内部のアドレスには接続しません")

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func isInternal(ip netip.Addr) bool {
	ip = ip.Unmap()
	return !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() ||
		ip.IsUnspecified() || cgnat.Contains(ip)
}

func denyInternal(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil || isInternal(ap.Addr()) {
		return errInternalAddr
	}
	return nil
}

// allowInternalPages はテストで手元のサーバに取りに行くときだけ true にする。
var allowInternalPages = false

func newPageClient() *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second}
	if !allowInternalPages {
		d.Control = denyInternal
	}
	return &http.Client{Transport: &http.Transport{
		Proxy:                 nil,
		DialContext:           d.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: pageTimeout,
		MaxIdleConnsPerHost:   2,
	}}
}
