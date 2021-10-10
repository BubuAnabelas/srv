package main

import (
	"archive/zip"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/joshuarli/srv/internal/humanize"
)

type context struct {
	srvDir string
}

// We write the shortest browser-valid base64 data string,
// so that the browser does not request the favicon.
const listingPrelude = `<head><link rel=icon href=data:,><style>* { font-family: monospace; } table { border: none; margin: 1rem; } td { padding-right: 2rem; }</style></head>
<table>`

func isZip(s string) bool {
	return mime.TypeByExtension(filepath.Ext(s)) == mime.TypeByExtension(".zip")
}

func renderListing(w http.ResponseWriter, r *http.Request, f *os.File) error {
	files, err := f.Readdir(-1)
	if err != nil {
		return err
	}

	io.WriteString(w, listingPrelude)

	sort.Slice(files, func(i, j int) bool {
		// TODO: add switch to make case sensitive
		// TODO: add switch to disable natural sort
		return humanize.NaturalLess(
			strings.ToLower(files[i].Name()),
			strings.ToLower(files[j].Name()),
		)
	})

	var fn, fnEscaped string
	for _, fi := range files {
		fn = fi.Name()
		fnEscaped = url.PathEscape(fn)
		switch m := fi.Mode(); {
		// is a directory - render a link
		case m&os.ModeDir != 0:
			fmt.Fprintf(w, "<tr><td><a href=\"%s/\">%s/</a></td></tr>", fnEscaped, fn)
		// is a regular file - render both a link and a file size
		case m&os.ModeType == 0:
			fs := humanize.FileSize(fi.Size())
			fmt.Fprintf(w, "<tr><td><a href=\"%s\">%s</a></td><td>%s</td></tr>", fnEscaped, fn, fs)
		// otherwise, don't render a clickable link
		default:
			fmt.Fprintf(w, "<tr><td><p style=\"color: #777\">%s</p></td></tr>", fn)
		}
	}

	io.WriteString(w, "</table>")
	return nil
}

func renderZipFolderListing(w http.ResponseWriter, r *http.Request, f []fs.DirEntry, parentPath string) error {
	io.WriteString(w, listingPrelude)

	var fnEscaped string
	for _, fi := range f {
		fn := fi.Name()
		fnEscaped = path.Join(parentPath, url.PathEscape(fi.Name()))
		switch m := fi.Type(); {
		// is a directory - render a link
		case m&os.ModeDir != 0:
			fmt.Fprintf(w, "<tr><td><a href=\"/%s\">%s</a></td></tr>", fnEscaped, fn)
		// is a regular file - render both a link and a file size
		case m&os.ModeType == 0:
			finfo, _ := fi.Info()
			fs := humanize.FileSize(finfo.Size())
			fmt.Fprintf(w, "<tr><td><a href=\"/%s\">%s</a></td><td>%s</td></tr>", fnEscaped, fn, fs)
		// otherwise, don't render a clickable link
		default:
			fmt.Fprintf(w, "<tr><td><p style=\"color: #777\">%s</p></td></tr>", fn)
		}
	}

	io.WriteString(w, "</table>")
	return nil
}

func renderZipListing(w http.ResponseWriter, r *http.Request, f zip.Reader, parentPath string) error {

	io.WriteString(w, listingPrelude)
	fmt.Fprint(w, "<tr><td><a href=?download>download zip</a></td></tr>")

	var fnEscaped string
	for _, fi := range f.File {
		fn := fi.Name
		fnEscaped = path.Join(parentPath, url.PathEscape(fi.Name))
		switch m := fi.Mode(); {
		// is a directory - render a link
		case m&os.ModeDir != 0 && len(strings.Split(strings.TrimSuffix(fn, "/"), "/")) == 1:
			fmt.Fprintf(w, "<tr><td><a href=\"/%s\">%s</a></td></tr>", fnEscaped, fn)
		// is a regular file - render both a link and a file size
		case m&os.ModeType == 0 && len(strings.Split(fn, "/")) == 1:
			fs := humanize.FileSize(int64(fi.UncompressedSize64))
			fmt.Fprintf(w, "<tr><td><a href=\"/%s\">%s</a></td><td>%s</td></tr>", fnEscaped, fn, fs)
			// otherwise, don't render a clickable link
			//default:
			//	fmt.Fprintf(w, "<tr><td><p style=\"color: #777\">%s</p></td></tr>", fn)
		}
	}

	io.WriteString(w, "</table>")
	return nil
}

func (c *context) handler(w http.ResponseWriter, r *http.Request) {
	// TODO: better log styling
	log.Printf("\t%s [%s]: %s %s %s", r.RemoteAddr, r.UserAgent(), r.Method, r.Proto, r.Host+r.RequestURI)

	// Tell HTTP 1.1+ clients to not cache responses.
	w.Header().Set("Cache-Control", "no-store")

	switch r.Method {
	case http.MethodGet:
		// Filenames could contain special uri characters, so we use r.RequestURI
		// instead of r.URL.Path.
		// XXX: Might also have to do QueryUnescape (and then also QueryEscape in the renderer),
		// but haven't run into that as a need in my usage.
		fp, err := url.PathUnescape(r.URL.Path)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to path unescape: %s", err), http.StatusInternalServerError)
			return
		}

		dirs := strings.Split(fp, "/")
		var fsPath []string
		var zipFile string
		var zipPath []string
		if len(dirs) > 0 {
			for _, fpath := range dirs {
				if len(zipFile) == 0 {
					if isZip(fpath) {
						zipFile = fpath
					} else {
						fsPath = append(fsPath, fpath)
					}
				} else {
					zipPath = append(zipPath, fpath)
				}
			}
		}

		if len(zipFile) > 0 {
			zipFilePath := path.Join(c.srvDir, path.Join(append(fsPath, zipFile)...))
			z, err := zip.OpenReader(zipFilePath)
			if err != nil {
				log.Fatal(err)
			}
			defer z.Close()

			_, isDownload := r.URL.Query()["download"]

			if isDownload {
				fp, _ := filepath.Abs(fp)
				f, _ := os.Open(fp)
				defer f.Close()
				http.ServeContent(w, r, fp, time.Time{}, f)
			} else if len(zipPath) > 0 {
				zipInternalPath := path.Join(zipPath...)
				f, _ := z.Open(zipInternalPath)
				defer f.Close()
				fi, _ := fs.Stat(z, zipInternalPath)
				if fi.IsDir() {
					fdir, _ := fs.ReadDir(z, zipInternalPath)
					err = renderZipFolderListing(w, r, fdir, path.Join(zipFilePath, zipInternalPath))
				} else {
					io.Copy(w, f)
				}
			} else {
				err = renderZipListing(w, r, z.Reader, zipFilePath)
			}

			return
		}

		fp = path.Join(c.srvDir, fp)

		fi, err := os.Lstat(fp)
		if err != nil {
			// NOTE: errors.Is is generally preferred, since it can unwrap errors created like so:
			//     fmt.Errorf("can't read file: %w", err)
			// But in this case we just want to check right after a stat.
			if os.IsNotExist(err) {
				http.Error(w, "file not found", http.StatusNotFound)
				return
			}
			http.Error(w, fmt.Sprintf("failed to stat file: %s", err), http.StatusInternalServerError)
			return
		}

		f, err := os.Open(fp)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to open file: %s", err), http.StatusInternalServerError)
			return
		}
		defer f.Close()

		switch m := fi.Mode(); {
		// is a directory - serve an index.html if it exists, otherwise generate and serve a directory listing
		case m&os.ModeDir != 0:
			// XXX: if a symlink has name "index.html", it will be served here.
			// i could add an extra lstat here, but the scenario is just too rare
			// to justify the additional file operation.
			html, err := os.Open(path.Join(fp, "index.html"))
			if err == nil {
				io.Copy(w, html)
				html.Close()
				return
			}
			html.Close()
			err = renderListing(w, r, f)
			if err != nil {
				http.Error(w, "failed to render directory listing: "+err.Error(), http.StatusInternalServerError)
			}
		// is a regular file - serve its contents
		case m&os.ModeType == 0:
			// This deduces a mimetype from the file extension first, then falls back to DetectContentType.
			// io.Copy'ing would only DetectContentType, which is insufficient for like, css files.
			http.ServeContent(w, r, fp, time.Time{}, f)
		// is a symlink - refuse to serve
		case m&os.ModeSymlink != 0:
			// TODO: add a flag to allow serving symlinks
			http.Error(w, "file is a symlink", http.StatusForbidden)
		default:
			http.Error(w, "file isn't a regular file or directory", http.StatusForbidden)
		}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func die(format string, v ...interface{}) {
	fmt.Fprintf(os.Stderr, format, v...)
	os.Stderr.Write([]byte("\n"))
	os.Exit(1)
}

// VERSION passed at build time
var VERSION = "unknown"

func main() {
	flag.Usage = func() {
		die(`srv %s (go version %s)

usage: %s [-q] [-p port] [-c certfile -k keyfile] directory

directory       path to directory to serve (default: .)

-q              quiet; disable all logging
-p port         port to listen on (default: 8000)
-b address      listener socket's bind address (default: 127.0.0.1)
-c certfile     optional path to a PEM-format X.509 certificate
-k keyfile      optional path to a PEM-format X.509 key
`, VERSION, runtime.Version(), os.Args[0])
	}

	var quiet bool
	var port, bindAddr, certFile, keyFile string
	flag.BoolVar(&quiet, "q", false, "")
	flag.StringVar(&port, "p", "8000", "")
	flag.StringVar(&bindAddr, "b", "127.0.0.1", "")
	flag.StringVar(&certFile, "c", "", "")
	flag.StringVar(&keyFile, "k", "", "")
	flag.Parse()

	certFileSpecified := certFile != ""
	keyFileSpecified := keyFile != ""
	if certFileSpecified != keyFileSpecified {
		die("You must specify both -c certfile -k keyfile.")
	}

	listenAddr := net.JoinHostPort(bindAddr, port)
	_, err := net.ResolveTCPAddr("tcp", listenAddr)
	if err != nil {
		die("Could not resolve the address to listen to: %s", listenAddr)
	}

	srvDir := "."
	posArgs := flag.Args()
	if len(posArgs) > 0 {
		srvDir = posArgs[0]
	}
	f, err := os.Open(srvDir)
	if err != nil {
		die(err.Error())
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.IsDir() {
		die("%s isn't a directory.", srvDir)
	}

	c := &context{
		srvDir: srvDir,
	}

	if quiet {
		log.SetFlags(0) // disable log formatting to save cpu
		log.SetOutput(io.Discard)
	}

	http.HandleFunc("/", c.handler)

	if certFileSpecified && keyFileSpecified {
		log.Printf("\tServing %s over HTTPS on %s", srvDir, listenAddr)
		err = http.ListenAndServeTLS(listenAddr, certFile, keyFile, nil)
	} else {
		log.Printf("\tServing %s over HTTP on %s", srvDir, listenAddr)
		err = http.ListenAndServe(listenAddr, nil)
	}

	die(err.Error())
}
