package encoding

import (
	"strings"

	"github.com/xtls/xray-core/common/net"
	"google.golang.org/grpc/metadata"
)

func forwardedClientAddress(md metadata.MD) net.Address {
	for _, key := range []string{"x-real-ip", "x-forwarded-for"} {
		for _, value := range md.Get(key) {
			for _, part := range strings.Split(value, ",") {
				addr := net.ParseAddress(strings.TrimSpace(part))
				if addr.Family().IsIP() {
					return addr
				}
			}
		}
	}

	return nil
	}
