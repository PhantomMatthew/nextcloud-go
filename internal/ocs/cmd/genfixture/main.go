package main

import (
	"fmt"
	"os"

	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
)

func main() {
	regex := `\/(\s|\n|^)(https?:\/\/)((?:[-A-Z0-9+_]+\.)+[-A-Z]+(?:\/[-A-Z0-9+&@#%?=~_|!:,.;()]*)*)(\s|\n|$)/i`
	data := ocs.Obj(
		ocs.K("capabilities", ocs.Obj(
			ocs.K("core", ocs.Obj(
				ocs.K("pollinterval", 60),
				ocs.K("webdav-root", "remote.php/webdav"),
				ocs.K("reference-api", true),
				ocs.K("reference-regex", regex),
			)),
		)),
	)
	body, _, err := ocs.Render(ocs.V2, ocs.FormatJSON, ocs.Meta{StatusCode: 200}, data)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Stdout.Write(body)
	fmt.Fprintf(os.Stderr, "\nlen=%d\n", len(body))
}
