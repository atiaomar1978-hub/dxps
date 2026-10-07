// dxpsctl is the DxPS operations CLI: secrets, certificates, migrations, seed data, topics and tokens.
package main

import (
	"fmt"
	"os"
)

var commands = map[string]func(args []string) error{
	"secrets":    cmdSecrets,
	"certs":      cmdCerts,
	"migrate":    cmdMigrate,
	"seed":       cmdSeed,
	"topics":     cmdTopics,
	"token":      cmdToken,
	"redrive":    cmdRedrive,
	"api":        cmdAPI,
	"testreport": cmdTestReport,
	"trace":      cmdTrace,
}

func main() {
	if len(os.Args) < 2 || commands[os.Args[1]] == nil {
		fmt.Fprintln(os.Stderr, "usage: dxpsctl secrets|certs|migrate|seed|topics|token|redrive|api|testreport|trace [flags]")
		os.Exit(2)
	}
	if err := commands[os.Args[1]](os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "dxpsctl:", err)
		os.Exit(1)
	}
}
