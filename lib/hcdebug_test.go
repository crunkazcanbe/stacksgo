package lib

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

const hcTestYAML = `name: test
services:
  jellystat:
    image: cyfershepard/jellystat:latest
    healthcheck:
      test:
        - "CMD-SHELL"
        - "wget -qO- http://localhost/ || exit 1"
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 30s
    container_name: jellystat
    environment:
      - "POSTGRES_USER=jellystat"
      - "POSTGRES_PORT=5432"
      - "TZ=America/Chicago"
    volumes:
      - "/x:/y"
    restart: "no"
`

func TestHCDebug(t *testing.T) {
	f, _ := os.CreateTemp("", "hc*.yml")
	f.WriteString(hcTestYAML)
	f.Close()
	defer os.Remove(f.Name())

	svcs, lines, err := fxParseServicesWithPositions(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range svcs {
		fmt.Printf("SVC %q: blockStart=%d blockEnd=%d hasHC=%v image=%q\n",
			s.name, s.blockStart, s.blockEnd, s.hasHealthcheck, s.image)
	}
	fmt.Println("---- ORIGINAL LINES (indexed) ----")
	for i, l := range lines {
		fmt.Printf("%2d| %s", i, l)
	}

	hc := hcResult{cmd: []string{"CMD-SHELL", "curl -sf http://localhost:3001/ || exit 1"},
		interval: "30s", timeout: "10s", retries: 3, start: "30s", source: "test"}

	fmt.Println("\n==== fxReplaceHCInService (force re-stamp path) ====")
	out, ok := fxReplaceHCInService(lines, svcs[0], hc)
	fmt.Printf("ok=%v\n%s\n", ok, strings.Join(out, ""))

	fmt.Println("==== fxInjectHCIntoService (add path) ====")
	out2, src := fxInjectHCIntoService(lines, svcs[0], false)
	fmt.Printf("src=%q\n%s\n", src, strings.Join(out2, ""))
}
