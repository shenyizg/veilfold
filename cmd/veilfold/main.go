// Command veilfold hides and locks private folders behind a password.
//
// Run "veilfold help" for usage.
package main

import (
	"os"

	"github.com/shenyizg/veilfold/internal/cli"
)

func main() {
	os.Exit(cli.Main())
}
