package db

import (
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/tinyrange/pkg2/memtar"
	"go.starlark.net/starlark"
)

type File interface {
	io.Reader
}

type StarFile struct {
	source FileSource
	f      File
	name   string
}

// Attr implements starlark.HasAttrs.
func (f *StarFile) Attr(name string) (starlark.Value, error) {
	if name == "read" {
		return starlark.NewBuiltin("File.read", func(
			thread *starlark.Thread,
			fn *starlark.Builtin,
			args starlark.Tuple,
			kwargs []starlark.Tuple,
		) (starlark.Value, error) {
			contents, err := io.ReadAll(f.f)
			if err != nil {
				return nil, fmt.Errorf("failed to read file: %s", err)
			}

			return starlark.String(contents), nil
		}), nil
	} else if name == "read_archive" {
		return starlark.NewBuiltin("File.read_archive", func(
			thread *starlark.Thread,
			fn *starlark.Builtin,
			args starlark.Tuple,
			kwargs []starlark.Tuple,
		) (starlark.Value, error) {
			var (
				ext             string
				stripComponents int
			)

			if err := starlark.UnpackArgs("File.read_archive", args, kwargs,
				"ext", &ext,
				"strip_components?", &stripComponents,
			); err != nil {
				return starlark.None, err
			}

			reader, err := ReadArchive(f.f, ext, stripComponents)
			if err != nil {
				return starlark.None, fmt.Errorf("failed to read archive: %s", err)
			}

			return &StarArchive{source: ExtractArchiveSource{
				Kind:            "ExtractArchive",
				Source:          f.source,
				Extension:       ext,
				StripComponents: stripComponents,
			}, r: reader, name: f.name}, nil
		}), nil
	} else if name == "read_compressed" {
		return starlark.NewBuiltin("File.read_compressed", func(
			thread *starlark.Thread,
			fn *starlark.Builtin,
			args starlark.Tuple,
			kwargs []starlark.Tuple,
		) (starlark.Value, error) {
			var (
				ext string
			)

			if err := starlark.UnpackArgs("File.read_compressed", args, kwargs,
				"ext", &ext,
			); err != nil {
				return starlark.None, err
			}

			if strings.HasSuffix(ext, ".gz") {
				r, err := gzip.NewReader(f.f)
				if err != nil {
					return nil, fmt.Errorf("failed to read compressed")
				}
				return &StarFile{source: DecompressSource{
					Kind:      "Decompress",
					Source:    f.source,
					Extension: ".gz",
				}, f: r, name: strings.TrimSuffix(f.name, ext)}, nil
			} else if strings.HasSuffix(ext, ".bz2") {
				r := bzip2.NewReader(f.f)
				return &StarFile{source: DecompressSource{
					Kind:      "Decompress",
					Source:    f.source,
					Extension: ".bz2",
				}, f: r, name: strings.TrimSuffix(f.name, ext)}, nil
			} else if strings.HasSuffix(ext, ".zst") {
				r, err := zstd.NewReader(f.f)
				if err != nil {
					return nil, fmt.Errorf("failed to read compressed")
				}
				return &StarFile{source: DecompressSource{
					Kind:      "Decompress",
					Source:    f.source,
					Extension: ".zst",
				}, f: r, name: strings.TrimSuffix(f.name, ext)}, nil
			} else {
				return starlark.None, fmt.Errorf("unsupported extension: %s", ext)
			}
		}), nil
	} else if name == "read_rpm_xml" {
		return starlark.NewBuiltin("File.read_rpm_xml", func(
			thread *starlark.Thread,
			fn *starlark.Builtin,
			args starlark.Tuple,
			kwargs []starlark.Tuple,
		) (starlark.Value, error) {
			return rpmReadXml(thread, f.f)
		}), nil
	} else if name == "name" {
		return starlark.String(f.name), nil

	} else if name == "base" {
		return starlark.String(path.Base(f.name)), nil
	} else {
		return nil, nil
	}
}

// AttrNames implements starlark.HasAttrs.
func (*StarFile) AttrNames() []string {
	return []string{"read", "read_archive", "read_compressed", "read_rpm_xml", "name", "base"}
}

func (f *StarFile) String() string      { return fmt.Sprintf("File{%s}", f.name) }
func (*StarFile) Type() string          { return "File" }
func (*StarFile) Hash() (uint32, error) { return 0, fmt.Errorf("File is not hashable") }
func (*StarFile) Truth() starlark.Bool  { return starlark.True }
func (*StarFile) Freeze()               {}

var (
	_ starlark.Value    = &StarFile{}
	_ starlark.HasAttrs = &StarFile{}
)

type StarArchiveIterator struct {
	ents  []memtar.Entry
	index int
}

// Done implements starlark.Iterator.
func (it *StarArchiveIterator) Done() {

}

// Next implements starlark.Iterator.
func (it *StarArchiveIterator) Next(p *starlark.Value) bool {
	if it.index >= len(it.ents) {
		return false
	}

	ent := it.ents[it.index]

	*p = &StarFile{f: ent.Open(), name: ent.Filename()}

	it.index += 1

	return true
}

var (
	_ starlark.Iterator = &StarArchiveIterator{}
)

type StarArchive struct {
	source FileSource
	r      memtar.TarReader
	name   string
}

// Get implements starlark.Mapping.
func (ar *StarArchive) Get(k starlark.Value) (v starlark.Value, found bool, err error) {
	filename, _ := starlark.AsString(k)

	for _, ent := range ar.r.Entries() {
		if ent.Filename() == filename {
			return &StarFile{f: ent.Open(), name: ent.Filename()}, true, nil
		}
	}

	return starlark.None, false, nil
}

// Iterate implements starlark.Iterable.
func (ar *StarArchive) Iterate() starlark.Iterator {
	return &StarArchiveIterator{ents: ar.r.Entries()}
}

func (f *StarArchive) String() string      { return fmt.Sprintf("Archive{%s}", f.name) }
func (*StarArchive) Type() string          { return "StarArchive" }
func (*StarArchive) Hash() (uint32, error) { return 0, fmt.Errorf("StarArchive is not hashable") }
func (*StarArchive) Truth() starlark.Bool  { return starlark.True }
func (*StarArchive) Freeze()               {}

var (
	_ starlark.Value    = &StarArchive{}
	_ starlark.Iterable = &StarArchive{}
	_ starlark.Mapping  = &StarArchive{}
)
