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
	"time"

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
	sector   uint64
	size     uint64
	attached bool
	dev      losetup.Device
}

var opts struct {
	Verbose    bool   `short:"v"`
	DeviceName string `short:"d" long:"name" default:"sparse"`
	NoOp       bool   `short:"N"`
}

const DM_SECTOR_SIZE int = 512

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
	secPerBand := header.BandSize / uint64(DM_SECTOR_SIZE)
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
			sector: id * secPerBand,
			size:   uint64(info.Size()) / uint64(DM_SECTOR_SIZE),
		})
		return nil
	})

	if err != nil {
		log.Fatalln(err)
	}

	slices.SortFunc(bands, func(a, b band) int {
		return int(int64(a.sector) - int64(b.sector))
	})

	prog := progressbar.NewOptions(len(bands)+1,
		progressbar.OptionSetDescription("attaching"),
		progressbar.OptionSetMaxDetailRow(4),
	)

	table := make([]devmapper.Table, 0, len(bands))

	lastSec := uint64(0)
	for i, _ := range bands {
		prog.Add(1)
		devPath := "dummy"
		if opts.NoOp {
			time.Sleep(time.Millisecond * 1)
		} else {
			ldev, err := losetup.Attach(bands[i].path, 0, true)
			if err != nil {
				prog.AddDetail(fmt.Sprintf("failed to attach %x: %v", bands[i].id, err))
				continue
			}
			bands[i].dev = ldev
			devPath = ldev.Path()
			bands[i].attached = true
		}

		if lastSec != bands[i].sector {
			table = append(table, devmapper.ZeroTable{
				Start:  lastSec,
				Length: bands[i].sector - lastSec,
			})
		}

		table = append(table, devmapper.LinearTable{
			Start:         bands[i].sector,
			Length:        bands[i].size,
			BackendDevice: devPath,
		})

		lastSec = bands[i].sector + bands[i].size
	}

	prog.Exit()

	mapperActive := false

	if !opts.NoOp {
		err = devmapper.CreateAndLoad(opts.DeviceName, uuid.New().String(), devmapper.ReadOnlyFlag, table...)
		if err != nil {
			log.Println(err)
		} else {
			mapperActive = true
			log.Printf("Mapper device %s up and running. Press ^C to tear down.")
		}
	}

	waitCh := make(chan os.Signal)
	signal.Notify(waitCh, os.Interrupt)
	for {
		<-waitCh
		if mapperActive {
			err = devmapper.Remove(opts.DeviceName)
			if err != nil {
				log.Println(err)
				log.Printf("Device could not be removed. Press ^C to try again")
				continue
			}
			mapperActive = false
		}

		prog := progressbar.NewOptions(len(bands)+1,
			progressbar.OptionSetDescription("detaching"),
			progressbar.OptionSetMaxDetailRow(4),
		)

		anyFailures := false
		for i, _ := range bands {
			prog.Add(1)
			if bands[i].attached {
				err := bands[i].dev.Detach()
				if err != nil {
					anyFailures = true
					prog.AddDetail(fmt.Sprintf("failed to detach %x: %v", bands[i].id, err))
					continue
				}
				bands[i].dev.Remove()
				bands[i].attached = false
			}
		}

		prog.Exit()
		if anyFailures {
			log.Printf("failed to detach all loop devices")
			continue
		}

		os.Exit(0)
	}
}
