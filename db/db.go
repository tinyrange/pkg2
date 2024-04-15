package db

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	xj "github.com/basgys/goxml2json"
	"github.com/icza/dyno"
	"github.com/schollz/progressbar/v3"
	"github.com/tinyrange/pkg2/core"
	"github.com/tinyrange/pkg2/third_party/regexp"
	bolt "go.etcd.io/bbolt"
	starlarkjson "go.starlark.net/lib/json"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
	"gopkg.in/yaml.v3"
	"howett.net/plist"
)

type PackageDatabase struct {
	Eif             *core.EnvironmentInterface
	Fetchers        []*RepositoryFetcher
	ScriptFetchers  []*ScriptFetcher
	SearchProviders []*SearchProvider
	packageMap      map[string]*Package
	packageMapMutex sync.Mutex
	AllowLocal      bool
	ForceRefresh    bool
	NoParallel      bool
	PackageBase     string
	db              *bolt.DB
}

func (db *PackageDatabase) addRepositoryFetcher(distro string, f *starlark.Function, args starlark.Tuple) error {
	db.Fetchers = append(db.Fetchers, &RepositoryFetcher{
		db:      db,
		Distro:  distro,
		Func:    f,
		Args:    args,
		Counter: core.NewCounter(),
	})

	return nil
}

func (db *PackageDatabase) addScriptFetcher(name string, f *starlark.Function, args starlark.Tuple) error {
	db.ScriptFetchers = append(db.ScriptFetchers, &ScriptFetcher{
		db:   db,
		Name: name,
		Func: f,
		Args: args,
	})

	return nil
}

func (db *PackageDatabase) addSearchProvider(distro string, f *starlark.Function, args starlark.Tuple) error {
	db.SearchProviders = append(db.SearchProviders, &SearchProvider{
		db:           db,
		Distribution: distro,
		Func:         f,
		Args:         args,
	})

	return nil
}

func (db *PackageDatabase) getGlobals(name string) (starlark.StringDict, error) {
	globals := starlark.StringDict{
		"fetch_http": starlark.NewBuiltin("fetch_http", func(
			thread *starlark.Thread,
			fn *starlark.Builtin,
			args starlark.Tuple,
			kwargs []starlark.Tuple,
		) (starlark.Value, error) {
			var (
				url          string
				expectedSize int64
				accept       string
				useETag      bool
				fast         bool
				expireTime   int64
			)

			if err := starlark.UnpackArgs("fetch_http", args, kwargs,
				"url", &url,
				"expected_size?", &expectedSize,
				"accept?", &accept,
				"use_etag?", &useETag,
				"fast?", &fast,
				"expire_time?", &expireTime,
			); err != nil {
				return starlark.None, err
			}

			f, err := db.Eif.HttpGetReader(url, core.HttpOptions{
				ExpectedSize: expectedSize,
				Accept:       accept,
				UseETag:      useETag,
				FastDownload: fast,
				ExpireTime:   time.Duration(expireTime),
				Logger:       core.GetLogger(thread),
			})
			if err == core.ErrNotFound {
				return starlark.None, nil
			} else if err != nil {
				return starlark.None, err
			}

			return &StarFile{f: f, name: url}, nil
		}),
		"fetch_git": starlark.NewBuiltin("fetch_git", func(
			thread *starlark.Thread,
			fn *starlark.Builtin,
			args starlark.Tuple,
			kwargs []starlark.Tuple,
		) (starlark.Value, error) {
			var (
				url string
			)

			if err := starlark.UnpackArgs("fetch_git", args, kwargs,
				"url", &url,
			); err != nil {
				return starlark.None, err
			}

			return db.fetchGit(url)
		}),
		"register_script_fetcher": starlark.NewBuiltin("register_script_fetcher", func(
			thread *starlark.Thread,
			fn *starlark.Builtin,
			args starlark.Tuple,
			kwargs []starlark.Tuple,
		) (starlark.Value, error) {
			var (
				name  string
				f     *starlark.Function
				fArgs starlark.Tuple
			)

			if err := starlark.UnpackArgs("register_script_fetcher", args, kwargs,
				"name", &name,
				"f", &f,
				"fArgs", &fArgs,
			); err != nil {
				return starlark.None, err
			}

			if err := db.addScriptFetcher(name, f, fArgs); err != nil {
				return starlark.None, err
			}

			return starlark.None, nil
		}),
		"register_search_provider": starlark.NewBuiltin("register_search_provider", func(
			thread *starlark.Thread,
			fn *starlark.Builtin,
			args starlark.Tuple,
			kwargs []starlark.Tuple,
		) (starlark.Value, error) {
			var (
				distro string
				f      *starlark.Function
				fArgs  starlark.Tuple
			)

			if err := starlark.UnpackArgs("register_search_provider", args, kwargs,
				"distro", &distro,
				"f", &f,
				"fArgs", &fArgs,
			); err != nil {
				return starlark.None, err
			}

			if err := db.addSearchProvider(distro, f, fArgs); err != nil {
				return starlark.None, err
			}

			return starlark.None, nil
		}),
		"fetch_repo": starlark.NewBuiltin("fetch_repo", func(
			thread *starlark.Thread,
			fn *starlark.Builtin,
			args starlark.Tuple,
			kwargs []starlark.Tuple,
		) (starlark.Value, error) {
			var (
				distro string
				f      *starlark.Function
				fArgs  starlark.Tuple
			)

			if err := starlark.UnpackArgs("fetch_repo", args, kwargs,
				"f", &f,
				"fArgs", &fArgs,
				"distro?", &distro,
			); err != nil {
				return starlark.None, err
			}

			if err := db.addRepositoryFetcher(distro, f, fArgs); err != nil {
				return starlark.None, err
			}

			return starlark.None, nil
		}),
		"parse_shell": starlark.NewBuiltin("parse_shell", func(
			thread *starlark.Thread,
			fn *starlark.Builtin,
			args starlark.Tuple,
			kwargs []starlark.Tuple,
		) (starlark.Value, error) {
			var (
				contents string
			)

			if err := starlark.UnpackArgs("parse_shell", args, kwargs,
				"contents", &contents,
			); err != nil {
				return starlark.None, err
			}

			return parseShell(contents)
		}),
		"parse_yaml": starlark.NewBuiltin("parse_yaml", func(
			thread *starlark.Thread,
			fn *starlark.Builtin,
			args starlark.Tuple,
			kwargs []starlark.Tuple,
		) (starlark.Value, error) {
			var (
				contents string
			)

			if err := starlark.UnpackArgs("parse_yaml", args, kwargs,
				"contents", &contents,
			); err != nil {
				return starlark.None, err
			}

			var body interface{}
			if err := yaml.Unmarshal([]byte(contents), &body); err != nil {
				return starlark.None, err
			}

			body = dyno.ConvertMapI2MapS(body)

			if b, err := json.Marshal(body); err != nil {
				return starlark.None, err
			} else {
				return starlark.Call(
					thread,
					starlarkjson.Module.Members["decode"],
					starlark.Tuple{starlark.String(b)},
					[]starlark.Tuple{},
				)
			}
		}),
		"parse_xml": starlark.NewBuiltin("parse_xml", func(
			thread *starlark.Thread,
			fn *starlark.Builtin,
			args starlark.Tuple,
			kwargs []starlark.Tuple,
		) (starlark.Value, error) {
			var (
				contents string
			)

			if err := starlark.UnpackArgs("parse_xml", args, kwargs,
				"contents", &contents,
			); err != nil {
				return starlark.None, err
			}

			json, err := xj.Convert(strings.NewReader(contents))
			if err != nil {
				return starlark.None, err
			}

			return starlark.Call(
				thread,
				starlarkjson.Module.Members["decode"],
				starlark.Tuple{starlark.String(json.String())},
				[]starlark.Tuple{},
			)
		}),
		"parse_nix_derivation": starlark.NewBuiltin("parse_nix_derivation", func(
			thread *starlark.Thread,
			fn *starlark.Builtin,
			args starlark.Tuple,
			kwargs []starlark.Tuple,
		) (starlark.Value, error) {
			var (
				contents string
			)

			if err := starlark.UnpackArgs("parse_nix_derivation", args, kwargs,
				"contents", &contents,
			); err != nil {
				return starlark.None, err
			}

			return parseNixDerivation(thread, contents)
		}),
		"parse_plist": starlark.NewBuiltin("parse_plist", func(
			thread *starlark.Thread,
			fn *starlark.Builtin,
			args starlark.Tuple,
			kwargs []starlark.Tuple,
		) (starlark.Value, error) {
			var (
				contents string
			)

			if err := starlark.UnpackArgs("parse_plist", args, kwargs,
				"contents", &contents,
			); err != nil {
				return starlark.None, err
			}

			var obj any

			if _, err := plist.Unmarshal([]byte(contents), &obj); err != nil {
				return starlark.None, err
			}

			bytes, err := json.Marshal(obj)
			if err != nil {
				return starlark.None, err
			}

			return starlark.Call(
				thread,
				starlarkjson.Module.Members["decode"],
				starlark.Tuple{starlark.String(bytes)},
				[]starlark.Tuple{},
			)
		}),
		"eval_starlark": starlark.NewBuiltin("eval_starlark", func(
			thread *starlark.Thread,
			fn *starlark.Builtin,
			args starlark.Tuple,
			kwargs []starlark.Tuple,
		) (starlark.Value, error) {
			contents, ok := starlark.AsString(args[0])
			if !ok {
				return starlark.None, fmt.Errorf("could not convert %s to string", args[0].Type())
			}

			return evalStarlark(contents, kwargs)
		}),
		"eval_python": starlark.NewBuiltin("eval_python", func(
			thread *starlark.Thread,
			fn *starlark.Builtin,
			args starlark.Tuple,
			kwargs []starlark.Tuple,
		) (starlark.Value, error) {
			contents, ok := starlark.AsString(args[0])
			if !ok {
				return starlark.None, fmt.Errorf("could not convert %s to string", args[0].Type())
			}

			return evalPython(contents, kwargs)
		}),
		"open": starlark.NewBuiltin("open", func(
			thread *starlark.Thread,
			fn *starlark.Builtin,
			args starlark.Tuple,
			kwargs []starlark.Tuple,
		) (starlark.Value, error) {
			var (
				filename string
			)

			if err := starlark.UnpackArgs("open", args, kwargs,
				"filename", &filename,
			); err != nil {
				return starlark.None, err
			}

			if !db.AllowLocal {
				return starlark.None, fmt.Errorf("open is only allowed if -allowLocal is passed")
			}

			f, err := os.Open(filename)
			if err != nil {
				return starlark.None, err
			}

			return &StarFile{f: f}, nil
		}),
		"mutex": starlark.NewBuiltin("mutex", func(
			thread *starlark.Thread,
			fn *starlark.Builtin,
			args starlark.Tuple,
			kwargs []starlark.Tuple,
		) (starlark.Value, error) {
			return &StarMutex{}, nil
		}),
		"duration": starlark.NewBuiltin("duration", func(
			thread *starlark.Thread,
			fn *starlark.Builtin,
			args starlark.Tuple,
			kwargs []starlark.Tuple,
		) (starlark.Value, error) {
			var (
				hours        int64
				minutes      int64
				seconds      int64
				milliseconds int64
			)

			if err := starlark.UnpackArgs("duration", args, kwargs,
				"hours?", &hours,
				"minutes?", &minutes,
				"seconds?", &seconds,
				"milliseconds?", &milliseconds,
			); err != nil {
				return starlark.None, err
			}

			return starlark.MakeInt64(
				hours*int64(time.Hour) +
					minutes*int64(time.Minute) +
					seconds*int64(time.Second) +
					milliseconds*int64(time.Millisecond),
			), nil
		}),
		"error": starlark.NewBuiltin("error", func(
			thread *starlark.Thread,
			fn *starlark.Builtin,
			args starlark.Tuple,
			kwargs []starlark.Tuple,
		) (starlark.Value, error) {
			var (
				message string
			)

			if err := starlark.UnpackArgs("error", args, kwargs,
				"message", &message,
			); err != nil {
				return starlark.None, err
			}

			return starlark.None, fmt.Errorf("%s", message)
		}),
		"json":     starlarkjson.Module,
		"re":       regexp.Module,
		"__name__": starlark.String(name),
	}

	return globals, nil
}

func (db *PackageDatabase) LoadScript(filename string) error {
	thread := &starlark.Thread{
		Load: func(thread *starlark.Thread, module string) (starlark.StringDict, error) {
			globals, err := db.getGlobals(module)
			if err != nil {
				return nil, err
			}

			filename := filepath.Join(db.PackageBase, module)

			ret, err := starlark.ExecFileOptions(&syntax.FileOptions{
				TopLevelControl: true,
				Recursion:       true,
				Set:             true,
				GlobalReassign:  true,
			}, thread, filename, nil, globals)
			if err != nil {
				if sErr, ok := err.(*starlark.EvalError); ok {
					slog.Error("got starlark error", "error", sErr, "backtrace", sErr.Backtrace())
				}
				return nil, err
			}

			return ret, nil
		},
	}

	globals, err := db.getGlobals("__main__")
	if err != nil {
		return err
	}

	filename = filepath.Join(db.PackageBase, filename)

	_, err = starlark.ExecFileOptions(&syntax.FileOptions{
		TopLevelControl: true,
		Recursion:       true,
		Set:             true,
		GlobalReassign:  true,
	}, thread, filename, nil, globals)
	if err != nil {
		if sErr, ok := err.(*starlark.EvalError); ok {
			slog.Error("got starlark error", "error", sErr, "backtrace", sErr.Backtrace())
		}
		return err
	}

	return nil
}

func (db *PackageDatabase) FetchAll() error {
	wg := sync.WaitGroup{}
	done := make(chan bool)
	errors := make(chan error)

	pb := progressbar.Default(int64(len(db.Fetchers)))
	defer pb.Close()

	for _, fetcher := range db.Fetchers {
		key := fetcher.Key()

		wg.Add(1)
		if db.NoParallel {
			if err := fetcher.fetchWithKey(db.Eif, key, db.ForceRefresh); err != nil {
				return fmt.Errorf("failed to load %s: %s", fetcher.String(), err)
			}
		} else {
			go func(key string, fetcher *RepositoryFetcher) {
				defer wg.Done()
				if err := fetcher.fetchWithKey(db.Eif, key, db.ForceRefresh); err != nil {
					errors <- fmt.Errorf("failed to load %s: %s", fetcher.String(), err)
				}

				pb.Add(1)
			}(key, fetcher)
		}
	}

	go func() {
		wg.Wait()

		done <- true
	}()

	select {
	case err := <-errors:
		return err
	case <-done:
		break
	}

	// Get the package index.
	db.packageMapMutex.Lock()
	defer db.packageMapMutex.Unlock()
	db.packageMap = make(map[string]*Package)

	for _, fetcher := range db.Fetchers {
		for _, pkg := range fetcher.Packages {
			db.packageMap[pkg.Name.String()] = pkg
		}
	}

	return nil
}

// Start a series of go routines to automatically refresh each package fetcher every refreshTime.
func (db *PackageDatabase) StartAutoRefresh(maxParallelFetchers int, refreshTime time.Duration, forceRefresh bool) {
	// Initialize the package map.
	db.packageMap = make(map[string]*Package)

	updateRequests := make(chan struct {
		fetcher *RepositoryFetcher
		force   bool
	}, maxParallelFetchers)

	for _, fetcher := range db.Fetchers {
		go func(fetcher *RepositoryFetcher) {
			ticker := time.NewTicker(refreshTime)

			updateRequests <- struct {
				fetcher *RepositoryFetcher
				force   bool
			}{
				fetcher: fetcher,
				force:   forceRefresh,
			}

			for range ticker.C {
				updateRequests <- struct {
					fetcher *RepositoryFetcher
					force   bool
				}{
					fetcher: fetcher,
					force:   true,
				}
			}
		}(fetcher)
	}

	for i := 0; i < maxParallelFetchers; i++ {
		go func() {
			for {
				updateRequest := <-updateRequests

				key := updateRequest.fetcher.Key()

				if err := updateRequest.fetcher.fetchWithKey(db.Eif, key, updateRequest.force); err != nil {
					slog.Warn("could not get update fetcher", "fetcher", updateRequest.fetcher.String(), "error", err)
					continue
				}

				{
					db.packageMapMutex.Lock()

					for _, pkg := range updateRequest.fetcher.Packages {
						db.packageMap[pkg.Name.String()] = pkg
					}

					db.packageMapMutex.Unlock()
				}
			}
		}()
	}
}

func (db *PackageDatabase) searchWithProviders(query PackageName, maxResults int) ([]*Package, error) {
	for _, searchProvider := range db.SearchProviders {
		if query.Distribution != searchProvider.Distribution {
			continue
		}

		results, err := searchProvider.Search(query, maxResults)
		if err != nil {
			return nil, err
		}

		return results, nil
	}

	return []*Package{}, nil
}

func (db *PackageDatabase) Search(query PackageName, maxResults int) ([]*Package, error) {
	var ret []*Package

	slog.Info("search", "query", query)

outer:
	for _, fetcher := range db.Fetchers {
		// If the fetcher doesn't possibly match this query then early out.
		if !fetcher.Matches(query) {
			continue
		}

		// Search though each package.
		for _, pkg := range fetcher.Packages {
			if pkg.Matches(query) {
				ret = append(ret, pkg)
				if maxResults != 0 && len(ret) >= maxResults {
					break outer
				}
			}
		}
	}

	if len(ret) == 0 {
		return db.searchWithProviders(query, maxResults)
	} else {
		return ret, nil
	}
}

func (db *PackageDatabase) Get(key string) (*Package, bool) {
	db.packageMapMutex.Lock()
	defer db.packageMapMutex.Unlock()

	pkg, ok := db.packageMap[key]

	return pkg, ok
}

func (db *PackageDatabase) GetBuildScript(script BuildScript) (starlark.Value, error) {
	for _, fetcher := range db.ScriptFetchers {
		if fetcher.Name == script.Name {
			thread := &starlark.Thread{}

			args := starlark.Tuple{fetcher}
			args = append(args, fetcher.Args...)
			for _, arg := range script.Args {
				args = append(args, starlark.String(arg))
			}

			ret, err := starlark.Call(thread, fetcher.Func,
				args,
				[]starlark.Tuple{},
			)
			if err != nil {
				return nil, err
			}

			return ret, nil
		}
	}

	return nil, fmt.Errorf("no build script fetcher defined for: %s", script.Name)
}

type InstallationPlan struct {
	db        *PackageDatabase
	installed map[string]bool
	Packages  []*Package
}

func (plan *InstallationPlan) checkName(name PackageName) bool {
	_, ok := plan.installed[name.ShortName()]

	return ok
}

func (plan *InstallationPlan) addName(name PackageName) error {
	// if plan.checkName(name) {
	// 	return fmt.Errorf("%s is already installed", name.ShortName())
	// }

	plan.installed[name.ShortName()] = true

	return nil
}

type ErrPackageNotFound PackageName

// Error implements error.
func (e ErrPackageNotFound) Error() string {
	return fmt.Sprintf("package %s not found", PackageName(e).String())
}

var (
	_ error = ErrPackageNotFound{}
)

func (plan *InstallationPlan) addPackage(query PackageName) error {
	if plan.checkName(query) {
		// Already installed.
		return nil
	}

	// Only look for 1 package.
	results, err := plan.db.Search(query, 1)
	if err != nil {
		return err
	}
	if len(results) != 1 {
		return ErrPackageNotFound(query)
	}

	pkg := results[0]

	// Add the names to the installed list.
	if err := plan.addName(pkg.Name); err != nil {
		return err
	}

	for _, alias := range pkg.Aliases {
		if err := plan.addName(alias); err != nil {
			return err
		}
	}

	// Add all dependencies.
outer:
	for _, depend := range pkg.Depends {
		for _, option := range depend {
			err := plan.addPackage(option)
			if _, ok := err.(ErrPackageNotFound); ok {
				continue
			} else if err != nil {
				return err
			}

			continue outer
		}

		return fmt.Errorf("could not find installation candidate among options: %+v", depend)
	}

	// Finally add the package.
	plan.Packages = append(plan.Packages, pkg)

	return nil
}

func (db *PackageDatabase) MakeInstallationPlan(packages []PackageName) (*InstallationPlan, error) {
	plan := &InstallationPlan{db: db, installed: make(map[string]bool)}

	for _, pkg := range packages {
		if err := plan.addPackage(pkg); err != nil {
			return nil, err
		}
	}

	return plan, nil
}

func (db *PackageDatabase) Count() int64 {
	var ret int64 = 0

	for _, fetcher := range db.Fetchers {
		ret += int64(len(fetcher.Packages))
	}

	return ret
}

type FetcherStatus struct {
	Key            string
	Name           string
	Status         RepositoryFetcherStatus
	PackageCount   int
	LastUpdated    time.Time
	LastUpdateTime time.Duration
}

func (db *PackageDatabase) FetcherStatus() ([]FetcherStatus, error) {
	var ret []FetcherStatus

	for _, fetcher := range db.Fetchers {
		ret = append(ret, FetcherStatus{
			Key:            fetcher.Key(),
			Name:           fetcher.String(),
			Status:         fetcher.Status,
			PackageCount:   len(fetcher.Packages),
			LastUpdated:    fetcher.LastUpdated,
			LastUpdateTime: fetcher.LastUpdateTime,
		})
	}

	return ret, nil
}

func (db *PackageDatabase) GetFetcher(key string) (*RepositoryFetcher, error) {
	for _, fetcher := range db.Fetchers {
		if fetcher.Key() == key {
			return fetcher, nil
		}
	}

	return nil, fmt.Errorf("fetcher not found")
}

func (db *PackageDatabase) WriteNames(w io.Writer) error {
	enc := json.NewEncoder(w)

	for _, fetcher := range db.Fetchers {
		for _, pkg := range fetcher.Packages {
			if err := enc.Encode(pkg.Name); err != nil {
				return err
			}

			for _, alias := range pkg.Aliases {
				if err := enc.Encode(alias); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (db *PackageDatabase) AllNames() []PackageName {
	var ret []PackageName

	for _, fetcher := range db.Fetchers {
		for _, pkg := range fetcher.Packages {
			ret = append(ret, pkg.Name)
		}
	}

	return ret
}

func (db *PackageDatabase) DistributionList() []string {
	set := map[string]bool{"": true}

	for _, fetcher := range db.Fetchers {
		if fetcher.Distributions == nil {
			continue
		}

		for distro := range fetcher.Distributions {
			set[distro] = true
		}
	}

	var ret []string
	for name := range set {
		ret = append(ret, name)
	}
	slices.Sort(ret)
	return ret
}

func (db *PackageDatabase) ArchitectureList() []string {
	set := map[string]bool{"": true}

	for _, fetcher := range db.Fetchers {
		if fetcher.Architectures == nil {
			continue
		}

		for distro := range fetcher.Architectures {
			set[distro] = true
		}
	}

	var ret []string
	for name := range set {
		ret = append(ret, name)
	}
	slices.Sort(ret)
	return ret
}

func (db *PackageDatabase) OpenDatabase(filename string) (io.Closer, error) {
	var err error

	db.db, err = bolt.Open(filename, os.FileMode(0755), bolt.DefaultOptions)
	if err != nil {
		return nil, err
	}

	return db.db, nil
}
