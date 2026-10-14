package paths

import (
	"os"
	"strconv"
)

func uidString() string { return strconv.Itoa(os.Getuid()) }
