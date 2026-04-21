package mex

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

//go:embed regexp.txt
var volumeExpStrs string

func parseVolumeIndex(path string) *int {
	for _, expStr := range strings.Split(volumeExpStrs, "\n") {
		exp := regexp.MustCompile(expStr)
		if matches := exp.FindStringSubmatch(filepath.Base(path)); len(matches) >= 2 {
			if index, err := strconv.ParseInt(matches[1], 10, 32); err == nil {
				indexInt := int(index)
				return &indexInt
			}
		}
	}

	return nil
}

func isImagePath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".png", ".webp", ".bmp", ".gif", ".avif":
		return true
	default:
		return false
	}
}

func isArchivePath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".zip", ".cbz", ".rar", ".cbr", ".7z":
		return true
	default:
		return false
	}
}

func buildTemplatedName(pattern, path string, index, count int) (string, error) {
	var (
		paddingCount = math.Log10(float64(count))
		paddingFmt   = fmt.Sprintf("%%0.%dd", int(paddingCount+1))
	)

	context := struct {
		Index string
		Name  string
		Ext   string
	}{
		Index: fmt.Sprintf(paddingFmt, index),
		Name:  filepath.Base(path),
		Ext:   strings.ToLower(filepath.Ext(path)),
	}

	tmpl, err := template.New("name").Parse(pattern)
	if err != nil {
		return "", err
	}

	var buff bytes.Buffer
	if err := tmpl.Execute(&buff, context); err != nil {
		return "", err
	}

	return buff.String(), nil
}

type ExportFlags int

const (
	ExportFlag_CompressBook = 1 << iota
	ExportFlag_CompressVolumes
	ExportFlag_FlatPack
)

type ExportConfig struct {
	Flags          ExportFlags
	PageTemplate   string
	VolumeTemplate string
	BookTemplate   string
	Workers        int
}

type Page struct {
	Node   *Node
	Volume *Volume
	Index  int
}

func (self *Page) export(dir string, config ExportConfig) error {
	name, err := buildTemplatedName(config.PageTemplate, self.Node.Name, self.Index+1, len(self.Volume.Pages))
	if err != nil {
		return err
	}

	if err := copyFile(filepath.Join(dir, name), self.Node.Path); err != nil {
		return err
	}

	return nil
}

type Volume struct {
	Node  *Node
	Book  *Book
	Pages []*Page
	Index int

	avgSize int
	hash    []byte
}

func (self *Volume) export(path string, config ExportConfig, allocator *TempDirAllocator) error {
	name, err := buildTemplatedName(config.VolumeTemplate, stripExt(self.Node.Name), self.Index, self.Book.VolumeCount)
	if err != nil {
		return err
	}

	var (
		compress  = config.Flags&ExportFlag_CompressVolumes != 0
		outputDir = path
	)

	if compress {
		if outputDir, err = allocator.TempDir(); err != nil {
			return err
		}
	} else {
		outputDir = filepath.Join(outputDir, name)
		if err := os.MkdirAll(outputDir, 0755); err != nil {
			return err
		}
	}

	for _, page := range self.Pages {
		if err := page.export(outputDir, config); err != nil {
			return err
		}
	}

	if compress {
		archivePath := filepath.Join(path, name)
		if err := Compress(archivePath, outputDir); err != nil {
			return err
		}
	}

	return nil
}

func (self *Volume) compare(other *Volume) int {
	if len(self.Pages) > len(other.Pages) {
		return 1
	} else if len(self.Pages) < len(other.Pages) {
		return -1
	}

	if self.avgSize > other.avgSize {
		return 1
	} else if self.avgSize < other.avgSize {
		return -1
	}

	return bytes.Compare(self.hash, other.hash)
}

type Book struct {
	Node        *Node
	Volumes     map[int]*Volume
	VolumeCount int

	orphans []*Volume
}

func (self *Book) Export(path string, config ExportConfig, allocator *TempDirAllocator) error {
	name, err := buildTemplatedName(config.BookTemplate, stripExt(self.Node.Name), 0, 0)
	if err != nil {
		return err
	}

	var (
		compress  = config.Flags&ExportFlag_CompressBook != 0
		outputDir = path
	)

	if compress {
		if outputDir, err = allocator.TempDir(); err != nil {
			return err
		}
	} else {
		outputDir = filepath.Join(outputDir, name)
		if err := os.MkdirAll(outputDir, 0755); err != nil {
			return err
		}
	}

	var (
		volumeChan    = make(chan *Volume, 4)
		volumeErr     error
		volumeErrLock sync.Mutex
		volumeWg      sync.WaitGroup
	)

	for i := 0; i < cap(volumeChan); i++ {
		volumeWg.Add(1)
		go func() {
			defer volumeWg.Done()
			for volume := range volumeChan {
				if err := volume.export(outputDir, config, allocator); err != nil {
					volumeErrLock.Lock()
					volumeErr = err
					volumeErrLock.Unlock()
					break
				}
			}
		}()
	}

	for _, volume := range self.Volumes {
		volumeChan <- volume
	}

	close(volumeChan)
	volumeWg.Wait()

	if volumeErr != nil {
		return volumeErr
	}

	if compress {
		archivePath := filepath.Join(path, name)
		if err := Compress(archivePath, outputDir); err != nil {
			return err
		}
	}

	return nil
}

func (self *Book) addVolume(newVolume *Volume) {
	insert := func(v *Volume) {
		self.Volumes[v.Index] = v
		if v.Index >= self.VolumeCount {
			self.VolumeCount = v.Index + 1
		}
	}

	currVolume, _ := self.Volumes[newVolume.Index]
	if currVolume == nil {
		insert(newVolume)
	} else {
		switch currVolume.compare(newVolume) {
		case 1:
			self.addOrphan(newVolume)
		case -1:
			self.addOrphan(currVolume)
			insert(newVolume)
		}
	}
}

func (self *Book) addOrphan(newVolume *Volume) {
	for _, volume := range self.orphans {
		if volume.compare(newVolume) == 0 {
			return
		}
	}

	self.orphans = append(self.orphans, newVolume)
}

func (self *Book) parseVolumes(node *Node) error {
	if !node.Info.IsDir() {
		return nil
	}

	volume := &Volume{
		Node: node,
		Book: self,
	}

	var pageIndex int
	for _, child := range node.Children {
		if child.Info.IsDir() {
			if err := self.parseVolumes(child); err != nil {
				return err
			}
		} else if isImagePath(child.Name) {
			volume.Pages = append(volume.Pages, &Page{child, volume, pageIndex})
			pageIndex++
		}
	}

	if len(volume.Pages) == 0 {
		return nil
	}

	sort.Slice(volume.Pages, func(i, j int) bool {
		return strings.Compare(volume.Pages[i].Node.Name, volume.Pages[j].Node.Name) < 0
	})

	var (
		hasher    = sha256.New()
		totalSize = 0
	)

	for _, page := range volume.Pages {
		fp, err := os.Open(page.Node.Path)
		if err != nil {
			return err
		}

		size, err := io.Copy(hasher, fp)
		fp.Close()

		if err != nil {
			return err
		}

		totalSize += int(size)
	}

	volume.avgSize = totalSize / len(volume.Pages)
	volume.hash = hasher.Sum(nil)

	if index := parseVolumeIndex(node.Name); index != nil {
		volume.Index = *index
		self.addVolume(volume)
	} else {
		self.addOrphan(volume)
	}

	return nil
}

func ParseBook(node *Node) (*Book, error) {
	book := Book{
		Node:    node,
		Volumes: make(map[int]*Volume),
	}

	book.parseVolumes(node)

	if len(book.orphans) > 0 {
		sort.Slice(book.orphans, func(i, j int) bool {
			return strings.Compare(book.orphans[i].Node.Name, book.orphans[j].Node.Name) < 0
		})

		for _, volume := range book.orphans {
			volume.Index = book.VolumeCount
			book.addVolume(volume)
		}

		book.orphans = nil
	}

	if len(book.Volumes) == 0 {
		return nil, errors.New("no volumes found")
	}

	return &book, nil
}

// FlatPackReport 记录 FlatPack 运行结果，最终打印汇总
type FlatPackReport struct {
	mu      sync.Mutex
	packed  []string
	skipped []string
	failed  []string
}

func (r *FlatPackReport) recordPacked(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.packed = append(r.packed, name)
}

func (r *FlatPackReport) recordSkipped(name, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.skipped = append(r.skipped, fmt.Sprintf("%s  [原因: %s]", name, reason))
}

func (r *FlatPackReport) recordFailed(name string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = append(r.failed, fmt.Sprintf("%s  [错误: %v]", name, err))
}

func (r *FlatPackReport) Print() {
	log.Printf("====== FlatPack 汇总 ======")
	log.Printf("已打包: %d 项", len(r.packed))
	for _, s := range r.packed {
		log.Printf("  [+] %s", s)
	}
	if len(r.skipped) > 0 {
		log.Printf("已跳过: %d 项", len(r.skipped))
		for _, s := range r.skipped {
			log.Printf("  [-] %s", s)
		}
	}
	if len(r.failed) > 0 {
		log.Printf("失败:   %d 项", len(r.failed))
		for _, s := range r.failed {
			log.Printf("  [!] %s", s)
		}
	}
	log.Printf("===========================")
}

// FlatPack 将 inputPath 下的每个直接子目录/压缩包独立打包为 CBZ（或目录），
// 命名规则：
//   - 只含图片的目录    → 目录名.cbz
//   - 含单个子目录的目录 → 用外层目录名（包装层透传）
//   - 同时含图片和子目录 → 图片打入外层名.cbz，子目录各自打包
//   - 压缩包文件        → 解压后按目录逻辑处理，用文件名（去扩展名）命名
func FlatPack(inputPath, outputDir string, config ExportConfig, allocator *TempDirAllocator) error {
	entries, err := os.ReadDir(inputPath)
	if err != nil {
		return err
	}

	report := &FlatPackReport{}

	for _, entry := range entries {
		entryPath := filepath.Join(inputPath, entry.Name())
		entryName := entry.Name()

		if entry.IsDir() {
			produced, err := flatPackDir(entryPath, outputDir, entryName, config, allocator)
			if err != nil {
				report.recordFailed(entryName, err)
			} else if !produced {
				report.recordSkipped(entryName, "目录内未找到任何图片")
			} else {
				report.recordPacked(entryName)
			}
		} else if isArchivePath(entryName) {
			bookName := stripExt(entryName)
			contentDir, err := Decompress(entryPath, allocator)
			if err != nil {
				report.recordFailed(entryName, err)
				continue
			}
			produced, err := flatPackDir(contentDir, outputDir, bookName, config, allocator)
			if err != nil {
				report.recordFailed(entryName, err)
			} else if !produced {
				report.recordSkipped(entryName, "解压后未找到任何图片")
			} else {
				report.recordPacked(bookName)
			}
		}
	}

	report.Print()
	return nil
}

func flatPackDir(dirPath, outputDir, bookName string, config ExportConfig, allocator *TempDirAllocator) (bool, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return false, err
	}

	var (
		images       []string
		subdirPaths  []string
		subdirNames  []string
		archivePaths []string
		archiveNames []string
	)

	for _, entry := range entries {
		fullPath := filepath.Join(dirPath, entry.Name())
		if entry.IsDir() {
			subdirPaths = append(subdirPaths, fullPath)
			subdirNames = append(subdirNames, entry.Name())
		} else if isImagePath(entry.Name()) {
			images = append(images, fullPath)
		} else if isArchivePath(entry.Name()) {
			archivePaths = append(archivePaths, fullPath)
			archiveNames = append(archiveNames, stripExt(entry.Name()))
		}
	}

	sort.Strings(images)

	produced := false
	if len(images) > 0 {
		if err := flatPackImages(images, outputDir, bookName, config, allocator); err != nil {
			return false, err
		}
		produced = true
	}

	totalChildren := len(subdirPaths) + len(archivePaths)

	if totalChildren == 0 {
		return produced, nil
	}

	// 只有单个子项（目录或压缩包）且无直接图片：这是"包装"层，透传外层名字
	if len(images) == 0 && totalChildren == 1 {
		if len(subdirPaths) == 1 {
			return flatPackDir(subdirPaths[0], outputDir, bookName, config, allocator)
		}
		// 单个压缩包，使用外层名
		contentDir, err := Decompress(archivePaths[0], allocator)
		if err != nil {
			return false, fmt.Errorf("decompressing %s: %w", archivePaths[0], err)
		}
		return flatPackDir(contentDir, outputDir, bookName, config, allocator)
	}

	// 多个子项，或子项与图片并存：每个子目录/压缩包用自己的名字
	for i, subdir := range subdirPaths {
		p, err := flatPackDir(subdir, outputDir, subdirNames[i], config, allocator)
		if err != nil {
			return false, err
		}
		if p {
			produced = true
		}
	}

	for i, archPath := range archivePaths {
		contentDir, err := Decompress(archPath, allocator)
		if err != nil {
			return false, fmt.Errorf("decompressing %s: %w", archPath, err)
		}
		p, err := flatPackDir(contentDir, outputDir, archiveNames[i], config, allocator)
		if err != nil {
			return false, err
		}
		if p {
			produced = true
		}
	}

	return produced, nil
}

func flatPackImages(images []string, outputDir, bookName string, config ExportConfig, allocator *TempDirAllocator) error {
	compress := config.Flags&ExportFlag_CompressVolumes != 0

	var (
		targetDir string
		err       error
	)

	if compress {
		if targetDir, err = allocator.TempDir(); err != nil {
			return err
		}
	} else {
		targetDir = filepath.Join(outputDir, bookName)
		if err := os.MkdirAll(targetDir, 0755); err != nil {
			return err
		}
	}

	for i, imgPath := range images {
		name, err := buildTemplatedName(config.PageTemplate, imgPath, i+1, len(images))
		if err != nil {
			return err
		}
		if err := copyFile(filepath.Join(targetDir, name), imgPath); err != nil {
			return err
		}
	}

	if compress {
		archivePath := filepath.Join(outputDir, bookName)
		if err := Compress(archivePath, targetDir); err != nil {
			return err
		}
	}

	return nil
}
