// Package version carries the build identity of the binary.
package version

// Version is the semantic version of this build. Overridden at build time with
// -ldflags "-X github.com/nizartuanku/topolight/internal/version.Version=x.y.z".
var Version = "0.4.1"

// Product is the human name.
const Product = "TopoLight"

// Port is the default console port. Unique across the Hexward line.
const Port = 8433
