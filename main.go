package main

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/jessevdk/go-flags"
	"howett.net/plist"

	_ "github.com/anatol/devmapper.go"
	_ "github.com/freddierice/go-losetup"
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
	id       uint64
	sector   uint64
	size     uint64
	attached bool
	loopdev  int
}

var opts struct {
	Verbose    bool   `short:"v"`
	DeviceName string `short:"d" long:"name"`
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

	fmt.Println(bands)
}
