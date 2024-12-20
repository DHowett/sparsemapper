package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jessevdk/go-flags"
	"howett.net/plist"

	"github.com/anatol/devmapper.go"
	"github.com/freddierice/go-losetup"
	"github.com/schollz/progressbar/v3"
)

type infoDictionary struct {
	InfoVersion    string `plist:"CFBundleInfoDictionaryVersion"`
	Version        int    `plist:"CFBundleVersion,omitempty"`
	DisplayVersion string `plist:"CFBundleDisplayVersion,omitempty"`
}

type sparseBundleHeader struct {
	infoDictionary
	BandSize            uint64 `plist:"band-size"`
	BackingStoreVersion int    `plist:"bundle-backingstore-version"`
	DiskImageBundleType string `plist:"diskimage-bundle-type"`
	Size                uint64 `plist:"size"`
}

type band struct {
	path     string
	id       uint64
	offset   uint64
	size     uint64
	attached bool
	dev      losetup.Device
}

type sparseBundle struct {
	h     sparseBundleHeader
	path  string
	bands []band
}

func readBundle(path string) (*sparseBundle, error) {
	info := filepath.Join(path, "Info.plist")
	b := &sparseBundle{path: path}

	f, err := os.Open(info)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := plist.NewDecoder(f)

	err = dec.Decode(&b.h)
	if err != nil {
		return nil, err
	}

	b.bands, err = enumBands(path, b.h.BandSize)
	if err != nil {
		return nil, err
	}

	return b, nil
}

func enumBands(bundlePath string, bandSizeInBytes uint64) ([]band, error) {
	bands := make([]band, 0, 32768)
	bandDir := filepath.Join(bundlePath, "bands")
	err := filepath.WalkDir(bandDir, func(path string, d fs.DirEntry, err error) error {
		if d.IsDir() {
			return nil
		}
		b := filepath.Base(path)
		id, err := strconv.ParseUint(b, 16, 64)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		bands = append(bands, band{
			path:   path,
			id:     uint64(id),
			offset: id * bandSizeInBytes,
			size:   uint64(info.Size()),
		})
		return nil
	})

	if err != nil {
		return nil, err
	}

	slices.SortFunc(bands, func(a, b band) int {
		return int(int64(a.offset) - int64(b.offset))
	})

	return bands, nil
}

var errCancelled = errors.New("cancelled")

func attachBandsAndPrepareTable(bands []band, bundleSize uint64, prog *progressbar.ProgressBar, ctx context.Context) ([]devmapper.Table, error) {
	table := make([]devmapper.Table, 0, len(bands))

	lastByte := uint64(0)
	for i, _ := range bands {
		select {
		case <-ctx.Done():
			log.Println("bailing out")
			return nil, errCancelled
		default:
		}

		prog.Add(1)
		devPath := "dummy"
		if opts.NoOp {
			time.Sleep(time.Millisecond * 1)
			if verbosity >= 2 {
				log.Printf("[no-op] attached %s", bands[i].path)
			}
		} else {
			ldev, err := losetup.Attach(bands[i].path, 0, true)
			if err != nil {
				log.Printf("failed to attach %x: %v", bands[i].id, err)
				continue
			}
			if verbosity >= 2 {
				log.Printf("attached %s as %s", bands[i].path, ldev.Path())
			}
			bands[i].dev = ldev
			devPath = ldev.Path()
			bands[i].attached = true
		}

		if lastByte != bands[i].offset {
			table = append(table, devmapper.ZeroTable{
				Start:  lastByte,
				Length: bands[i].offset - lastByte,
			})
		}

		table = append(table, devmapper.LinearTable{
			Start:         bands[i].offset,
			Length:        bands[i].size,
			BackendDevice: devPath,
		})

		lastByte = bands[i].offset + bands[i].size
	}

	if lastByte != bundleSize {
		table = append(table, devmapper.ZeroTable{
			Start:  lastByte,
			Length: bundleSize - lastByte,
		})
	}

	return table, nil
}

func detachBands(bands []band, prog *progressbar.ProgressBar) error {
	prog.Describe("detaching")
	prog.Reset()

	anyDetachFail := false
	for i, _ := range bands {
		prog.Add(1)
		if bands[i].attached {
			err := bands[i].dev.Detach()
			if err != nil {
				anyDetachFail = true
				log.Printf("failed to detach %s: %v", bands[i].dev.Path(), err)
				continue
			}
			if verbosity >= 2 {
				log.Printf("detached %s from %s", bands[i].path, bands[i].dev.Path())
			}
			bands[i].attached = false
		} else if opts.NoOp {
			if verbosity >= 2 {
				log.Printf("[no-op] detached %s", bands[i].path)
			}
			time.Sleep(time.Millisecond * 1)
		}
	}
	if anyDetachFail {
		return errors.New("failed to detach all bands")
	}
	return nil
}

var opts struct {
	Verbose    []bool `short:"v" description:"verbose (more is more)"`
	DeviceName string `short:"d" long:"name" description:"name of device-mapper device to create; if not specified, one will be generated"`
	NoOp       bool   `short:"N" description:"pretend"`
}

var verbosity int

func main() {
	p := flags.NewParser(&opts, flags.HelpFlag|flags.PrintErrors)
	p.Usage = "[options] sparsebundle"
	args, err := p.ParseArgs(os.Args)
	if err != nil {
		if !flags.WroteHelp(err) {
			log.Fatalln(err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "missing bundle path\n")
		p.WriteHelp(os.Stderr)
		os.Exit(1)
	}

	verbosity = len(opts.Verbose)

	path := args[1]
	bundle, err := readBundle(path)
	if err != nil {
		log.Fatalln(err)
	}

	var progressOutput io.Writer = os.Stderr
	if verbosity >= 2 {
		progressOutput = io.Discard
	}

	prog := progressbar.NewOptions(len(bundle.bands),
		progressbar.OptionSetDescription("attaching"),
		progressbar.OptionClearOnFinish(),
		progressbar.OptionShowCount(),
		progressbar.OptionShowIts(),
		progressbar.OptionSetWriter(progressOutput),
	)

	if opts.DeviceName == "" {
		n := ""
		base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		for _, r := range base {
			if unicode.IsLetter(r) {
				n += string(unicode.ToLower(r))
			} else {
				n += "_"
			}
		}
		opts.DeviceName = n
	}

	if verbosity >= 1 {
		log.Printf("preparing loopback devices for dm node `%s'", opts.DeviceName)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	table, err := attachBandsAndPrepareTable(bundle.bands, bundle.h.Size, prog, ctx)
	cancel()

	mapperActive := false
	if err == errCancelled {
		err = detachBands(bundle.bands, prog) // detach all attached bands
		if err != nil {
			log.Println("could not detach all loop devices; manual cleanup will be required")
			os.Exit(1)
		}
		os.Exit(0)
	}

	if verbosity >= 1 {
		log.Printf("creating device mapper node `%s' with a %d-entry table", opts.DeviceName, len(table))
	}

	if !opts.NoOp {
		err = devmapper.CreateAndLoad(opts.DeviceName, uuid.New().String(), devmapper.ReadOnlyFlag, table...)
		if err != nil {
			log.Println(err)
		} else {
			mapperActive = true
		}
	}

	if mapperActive || opts.NoOp {
		log.Printf("`%s' ready (%d entries). ^C to shut down.", opts.DeviceName, len(table))
	}

	waitCh := make(chan os.Signal)
	signal.Notify(waitCh, os.Interrupt)
	failures := 0
	for {
		<-waitCh

		if failures >= 2 {
			log.Println("giving up despite earlier failures")
			break
		}

		if mapperActive {
			if verbosity >= 1 {
				log.Printf("unmapping `%s'", opts.DeviceName)
			}
			err = devmapper.Remove(opts.DeviceName)
			if err != nil {
				log.Println(err)
				log.Printf("failed to unmap dm")
				continue
			}
			mapperActive = false
		}

		err = detachBands(bundle.bands, prog)

		if err != nil {
			switch failures {
			case 0:
				log.Printf("failed to detach all loop devices; ^C to try again")
			case 1:
				log.Printf("failed (again) to detach all loop devices; ^C to give up")
			}
			failures++
			continue
		} else {
			// reset counter for clean exit
			failures = 0
		}

		break
	}

	os.Exit(failures)
}
