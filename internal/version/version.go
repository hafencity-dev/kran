// Package version carries build information, set via -ldflags -X.
package version

import (
	"fmt"

	"github.com/hafencity-dev/kran/internal/proto"
)

var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

func Info() proto.VersionInfo {
	return proto.VersionInfo{
		Version:   Version,
		Commit:    Commit,
		BuildDate: BuildDate,
		API:       proto.APIVersion,
		Protocol:  fmt.Sprintf("%d.%d", proto.ProtocolMajor, proto.ProtocolMinor),
	}
}
