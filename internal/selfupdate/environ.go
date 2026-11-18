package selfupdate

import "os"

func syscallEnviron() []string { return os.Environ() }
