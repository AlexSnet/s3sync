package util

import (
	"fmt"
)

var (
	VERSION_NUMBER = fmt.Sprintf("%.02f", 1.0)
	VERSION        = VERSION_NUMBER
	COMMIT         = ""
	BUILD_TIME     = ""
)

func Version() string {
	v := VERSION
	if COMMIT != "" {
		v = v + "@" + COMMIT
	}
	if BUILD_TIME != "" {
		v += " (build at " + BUILD_TIME + ")"
	}
	return v
}
