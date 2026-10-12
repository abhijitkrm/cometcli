package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/qrxfer"
)

// qrCmd moves a small public file (a join or gentx file) off an
// air-gapped machine as QR codes, with nothing plugged into it.
func qrCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "qr",
		Short: "Move a small public file off an offline machine as QR codes (join and gentx files)",
	}
	var pngDir string
	var all bool
	show := &cobra.Command{
		Use:   "show <file>",
		Short: "Offline machine: show a file as QR codes to scan, one at a time",
		Long: `Compresses the file and shows it as a few QR codes, each carrying the
file's checksum. Scan them with a phone or camera on a connected machine
and give the result to cometcli qr read there. Files with key material
are refused: only public files (join, gentx) may leave this way.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			parts, err := qrxfer.Encode(filepath.Base(args[0]), data)
			if err != nil {
				return err
			}
			if pngDir != "" {
				if err := os.MkdirAll(pngDir, 0o755); err != nil {
					return err
				}
				for i, p := range parts {
					f := filepath.Join(pngDir, fmt.Sprintf("%s.qr%02d.png", filepath.Base(args[0]), i+1))
					if err := qrxfer.PNG(p, f); err != nil {
						return err
					}
				}
				fmt.Fprintf(w, "%d QR images → %s\n", len(parts), pngDir)
				return nil
			}
			in := bufio.NewReader(cmd.InOrStdin())
			for i, p := range parts {
				s, err := qrxfer.Terminal(p)
				if err != nil {
					return err
				}
				fmt.Fprintf(w, "\n%s  — part %d of %d (%s, %d bytes)\n%s", filepath.Base(args[0]), i+1, len(parts), filepath.Base(args[0]), len(data), s)
				if !all && i < len(parts)-1 {
					fmt.Fprint(w, "scan it, then press Enter for the next part ")
					if _, err := in.ReadString('\n'); err != nil {
						return nil
					}
				}
			}
			fmt.Fprintf(w, "\nall %d parts shown. On the connected machine: cometcli qr read\n", len(parts))
			return nil
		},
	}
	show.Flags().StringVar(&pngDir, "png", "", "write the codes as PNG images into this directory instead (to show full screen)")
	show.Flags().BoolVar(&all, "all", false, "print every code at once instead of one per Enter")

	var outDir string
	read := &cobra.Command{
		Use:   "read [images…]",
		Short: "Connected machine: reassemble a file from scanned QR codes (pasted text or photos)",
		Long: `Paste each scanned code's text, one per line, in any order (a phone's QR
scanner gives the text), or pass photos/screenshots of the codes. The file
is written only when every part is in and it matches its checksum.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			var a qrxfer.Assembler
			add := func(text, from string) error {
				isNew, err := a.Add(text)
				if err != nil {
					return fmt.Errorf("%s: %w", from, err)
				}
				got, n, name := a.Progress()
				if isNew {
					fmt.Fprintf(w, "✓ %s part — %d of %d\n", name, got, n)
				}
				return nil
			}
			for _, img := range args {
				text, err := qrxfer.DecodeImage(img)
				if err != nil {
					return err
				}
				if err := add(text, img); err != nil {
					return err
				}
			}
			if len(args) == 0 {
				fmt.Fprintln(w, "paste each scanned code (one per line), in any order; an empty line or EOF ends")
				sc := bufio.NewScanner(cmd.InOrStdin())
				sc.Buffer(make([]byte, 64<<10), 1<<20)
				for sc.Scan() {
					line := strings.TrimSpace(sc.Text())
					if line == "" {
						if _, n, _ := a.Progress(); n > 0 && len(a.Missing()) == 0 {
							break
						}
						continue
					}
					if err := add(line, "pasted line"); err != nil {
						fmt.Fprintf(w, "! %v — skipped\n", err)
						continue
					}
					if _, n, _ := a.Progress(); n > 0 && len(a.Missing()) == 0 {
						break
					}
				}
			}
			name, data, err := a.File()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(outDir, 0o755); err != nil {
				return err
			}
			p := filepath.Join(outDir, name)
			if _, err := os.Stat(p); err == nil {
				return fmt.Errorf("%s exists — not overwritten (use --out)", p)
			}
			if err := os.WriteFile(p, data, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(w, "✓ %s reassembled and verified (%d bytes) → %s\n", name, len(data), p)
			return nil
		},
	}
	read.Flags().StringVar(&outDir, "out", ".", "directory to write the file into")
	cmd.AddCommand(show, read)
	return cmd
}
