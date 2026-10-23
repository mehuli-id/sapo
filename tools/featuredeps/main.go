// featuredeps mencetak paket Debian dari manifest fitur untuk dipakai skrip
// build paket. Contoh: go run ./tools/featuredeps -dir features -activation always
// menghasilkan "systemd, nftables, ..." untuk baris Depends di paket .deb.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/mehuli-id/sapo/internal/agent"
)

func main() {
	dir := flag.String("dir", "features", "folder manifest fitur")
	activation := flag.String("activation", "always", "always | setup | optional")
	flag.Parse()

	features, err := agent.LoadFeatures(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "manifest fitur:", err)
		os.Exit(1)
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range features {
		if f.Activation != *activation {
			continue
		}
		for _, p := range f.Packages {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	fmt.Println(strings.Join(out, ", "))
}
