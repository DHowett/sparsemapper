package main

import (
	"fmt"
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

	bundle := args[1]
	info := filepath.Join(bundle, "Info.plist")
	var header sparseBundleHeader

	{
		f, err := os.Open(info)
		if err != nil {
			log.Fatalln(err)
		}
		defer f.Close()
		dec := plist.NewDecoder(f)
		err = dec.Decode(&header)
		if err != nil {
			log.Fatalln(err)
		}
	}

	bandDir := filepath.Join(bundle, "bands")
	bands := make([]band, 0, 32768)
	bandSizeInBytes := header.BandSize
	err = filepath.WalkDir(bandDir, func(path string, d fs.DirEntry, err error) error {
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
		log.Fatalln(err)
	}

	slices.SortFunc(bands, func(a, b band) int {
		return int(int64(a.offset) - int64(b.offset))
	})

	prog := progressbar.NewOptions(len(bands),
		progressbar.OptionSetDescription("attaching"),
		progressbar.OptionClearOnFinish(),
		progressbar.OptionShowCount(),
		progressbar.OptionShowIts(),
	)

	table := make([]devmapper.Table, 0, len(bands))

	lastByte := uint64(0)
	for i, _ := range bands {
		prog.Add(1)
		devPath := "dummy"
		if opts.NoOp {
			time.Sleep(time.Millisecond * 1)
		} else {
			ldev, err := losetup.Attach(bands[i].path, 0, true)
			if err != nil {
				log.Println("failed to attach %x: %v", bands[i].id, err)
				continue
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

	if lastByte != header.Size {
		table = append(table, devmapper.ZeroTable{
			Start:  lastByte,
			Length: header.Size - lastByte,
		})
	}

	mapperActive := false

	if opts.DeviceName == "" {
		n := ""
		base := strings.TrimSuffix(filepath.Base(bundle), filepath.Ext(bundle))
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

		prog := progressbar.NewOptions(len(bands),
			progressbar.OptionSetDescription("detaching"),
			progressbar.OptionClearOnFinish(),
			progressbar.OptionShowCount(),
			progressbar.OptionShowIts(),
		)

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
				bands[i].attached = false
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
