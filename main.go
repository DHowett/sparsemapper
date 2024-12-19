package main

import (
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

var opts struct {
	Verbose    bool   `short:"v"`
	DeviceName string `short:"d" long:"name"`
	NoOp       bool   `short:"N"`
}

func main() {
	args, err := flags.ParseArgs(&opts, os.Args)
	if len(args) < 2 {
		log.Fatalln("insufficient args; pass path to bundle")
	}
	if err != nil {
		log.Fatalln(err)
	}

	path := args[1]
	bundle, err := readBundle(path)

	prog := progressbar.NewOptions(len(bundle.bands),
		progressbar.OptionSetDescription("attaching"),
		progressbar.OptionClearOnFinish(),
		progressbar.OptionShowCount(),
		progressbar.OptionShowIts(),
	)

	table := make([]devmapper.Table, 0, len(bundle.bands))

	lastByte := uint64(0)
	for i, _ := range bundle.bands {
		prog.Add(1)
		devPath := "dummy"
		if opts.NoOp {
			time.Sleep(time.Millisecond * 1)
		} else {
			ldev, err := losetup.Attach(bundle.bands[i].path, 0, true)
			if err != nil {
				log.Printf("failed to attach %x: %v", bundle.bands[i].id, err)
				continue
			}
			bundle.bands[i].dev = ldev
			devPath = ldev.Path()
			bundle.bands[i].attached = true
		}

		if lastByte != bundle.bands[i].offset {
			table = append(table, devmapper.ZeroTable{
				Start:  lastByte,
				Length: bundle.bands[i].offset - lastByte,
			})
		}

		table = append(table, devmapper.LinearTable{
			Start:         bundle.bands[i].offset,
			Length:        bundle.bands[i].size,
			BackendDevice: devPath,
		})

		lastByte = bundle.bands[i].offset + bundle.bands[i].size
	}

	if lastByte != bundle.h.Size {
		table = append(table, devmapper.ZeroTable{
			Start:  lastByte,
			Length: bundle.h.Size - lastByte,
		})
	}

	mapperActive := false

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

	if !opts.NoOp {
		err = devmapper.CreateAndLoad(opts.DeviceName, uuid.New().String(), devmapper.ReadOnlyFlag, table...)
		if err != nil {
			log.Println(err)
		} else {
			mapperActive = true
			log.Printf("Mapper device %s up and running. ^C to shut down.", opts.DeviceName)
		}
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
			err = devmapper.Remove(opts.DeviceName)
			if err != nil {
				log.Println(err)
				log.Printf("failed to unmap dm")
				continue
			}
			mapperActive = false
		}

		prog.Describe("detaching")
		prog.Reset()

		anyDetachFail := false
		for i, _ := range bundle.bands {
			prog.Add(1)
			if bundle.bands[i].attached {
				err := bundle.bands[i].dev.Detach()
				if err != nil {
					anyDetachFail = true
					log.Printf("failed to detach %s: %v", bundle.bands[i].dev.Path(), err)
					continue
				}
				bundle.bands[i].attached = false
			} else if opts.NoOp {
				time.Sleep(time.Millisecond * 1)
			}
		}

		if anyDetachFail {
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
	e := 0
	if failures > 0 {
		e = 1
	}
	os.Exit(e)
}
