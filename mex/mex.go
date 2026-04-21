package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"

	"codeberg.org/foosoft/mex"
)

func processPath(inputPath, outputDir string, config mex.ExportConfig) error {
	var allocator mex.TempDirAllocator
	defer allocator.Cleanup()

	if len(outputDir) == 0 {
		var err error
		outputDir, err = os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
	}

	if config.Flags&mex.ExportFlag_FlatPack != 0 {
		return mex.FlatPack(inputPath, outputDir, config, &allocator)
	}

	rootNode, err := mex.Walk(inputPath, &allocator)
	if err != nil {
		return err
	}

	book, err := mex.ParseBook(rootNode)
	if err != nil {
		return err
	}

	if err := book.Export(outputDir, config, &allocator); err != nil {
		return err
	}

	return nil
}

func main() {
	var (
		compressBook    = flag.Bool("zip-book", false, "compress book as a cbz archive")
		compressVolumes = flag.Bool("zip-volume", true, "compress volumes as cbz archives")
		flatPack        = flag.Bool("flat-pack", false, "pack each subdirectory as a separate flat cbz, named after the directory")
		pageTemplate    = flag.String("label-page", "page_{{.Index}}{{.Ext}}", "page name template")
		volumeTemplate  = flag.String("label-volume", "vol_{{.Index}}", "volume name template")
		bookTemplate    = flag.String("label-book", "{{.Name}}", "book name template")
		workers         = flag.Int("workers", runtime.NumCPU(), "number of simultaneous workers")
		webpConvert     = flag.Bool("webp", false, "convert images to webp using magick before packing")
		webpQuality     = flag.Int("webp-quality", 80, "webp conversion quality (1-100)")
	)

	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: mex <input_path> [<output_dir>]")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr, "Templates:")
		fmt.Fprintln(os.Stderr, "  {{.Index}} - index of current volume or page")
		fmt.Fprintln(os.Stderr, "  {{.Name}} - original filename and extension")
		fmt.Fprintln(os.Stderr, "  {{.Ext}} - original extension only")
	}

	flag.Parse()

	config := mex.ExportConfig{
		PageTemplate:   *pageTemplate,
		VolumeTemplate: *volumeTemplate,
		BookTemplate:   *bookTemplate,
		Workers:        *workers,
		WebpConvert:    *webpConvert,
		WebpQuality:    *webpQuality,
	}

	if *compressBook {
		config.Flags |= mex.ExportFlag_CompressBook
	}
	if *compressVolumes {
		config.Flags |= mex.ExportFlag_CompressVolumes
	}
	if *flatPack {
		config.Flags |= mex.ExportFlag_FlatPack
	}

	if argc := flag.NArg(); argc >= 1 && argc <= 2 {
		inputPath := flag.Arg(0)

		var outputDir string
		if argc == 2 {
			outputDir = flag.Arg(1)
		}

		if err := processPath(inputPath, outputDir, config); err != nil {
			log.Fatal(err)
		}
	} else {
		flag.Usage()
		os.Exit(2)
	}
}
