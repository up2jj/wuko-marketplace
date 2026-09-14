package forwardproxy

import (
	"time"

	"github.com/up2jj/wuko-marketplace/plugin-src/http/localtls"
)

type certificateAuthority = localtls.Authority

func newCertificateAuthority(now time.Time) (*certificateAuthority, error) {
	return localtls.NewAuthority(now, "Wuko Forward Proxy")
}

func writeCACertificate(runDir string, certificate []byte) (string, error) {
	return localtls.WriteCertificate(runDir, ".wuko-forward-proxy-ca-*.pem", certificate)
}
