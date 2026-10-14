//go:build unit || integration

package repository

import (
	"fmt"
	"os"
)

const (
	pgDumperHelperModeEnv = "SUB2API_PG_DUMPER_HELPER_MODE"
	pgDumperHelperData    = "backup-data"
	pgDumperHelperPartial = "partial-backup"
)

func runPgDumperHelper(mode string) {
	switch mode {
	case "success":
		_, _ = fmt.Fprint(os.Stdout, pgDumperHelperData)
	case "partial-failure":
		_, _ = fmt.Fprint(os.Stdout, pgDumperHelperPartial)
		os.Exit(7)
	case "exit-success":
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(2)
	}
}
