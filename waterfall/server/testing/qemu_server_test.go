package testing

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/waterfall/net/qemu"
	waterfall_grpc "github.com/waterfall/proto/waterfall_go_grpc"
	"github.com/waterfall/testutils"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
)

var (
	runfiles string

	// test args
	launcher = flag.String("launcher", "", "The path to the emulator launcher")
	adbTurbo = flag.String("adb_turbo", "", "The path to abd.turbo binary")
	server   = flag.String("server", "", "The path to test server")

	contents             = testBytes(16 * 1024)
	modeDir  os.FileMode = 0755

	testDir = fs{
		path:  "zoo",
		files: []string{"a.txt", "b.txt"},
		dirs: []fs{
			fs{
				path: "zoo/bar",
			},
			fs{
				path:  "zoo/baz",
				files: []string{"d.txt"},
				dirs: []fs{
					fs{
						path:  "zoo/baz/qux",
						files: []string{"e.txt"},
					}}}}}
)

type fs struct {
	path  string
	files []string
	dirs  []fs
}

const emuWorkingDir = "images/session"

func init() {
	flag.Parse()

	if *launcher == "" || *adbTurbo == "" || *server == "" {
		log.Fatalf("launcher, adb and server args need to be provided")
	}

	wd, err := os.Getwd()
	if err != nil {
		panic("unable to get wd")
	}
	runfiles = runfilesRoot(wd)
}

func runfilesRoot(path string) string {
	sep := "qemu_server_test.runfiles/__main__"
	return path[0 : strings.LastIndex(path, sep)+len(sep)]
}

func testBytes(size uint32) []byte {
	bb := new(bytes.Buffer)
	var i uint32
	for i = 0; i < size; i += 4 {
		binary.Write(bb, binary.LittleEndian, i)
	}
	return bb.Bytes()
}

func makeTestFile(path string, bits []byte) error {
	if err := ioutil.WriteFile(path, bits, 0655); err != nil {
		return err
	}
	return nil
}

func makeFs(baseDir string, tree fs) ([]string, error) {
	seen := []string{}
	dirPath := filepath.Join(baseDir, tree.path)
	if tree.path != "" {
		seen = append(seen, tree.path)
		if err := os.Mkdir(dirPath, modeDir); err != nil {
			return nil, err
		}
	}
	for _, f := range tree.files {
		seen = append(seen, filepath.Join(tree.path, f))
		if err := makeTestFile(filepath.Join(dirPath, f), contents); err != nil {
			return nil, err
		}
	}
	for _, nfs := range tree.dirs {
		s, err := makeFs(baseDir, nfs)
		if err != nil {
			return nil, err
		}
		seen = append(seen, s...)
	}
	return seen, nil
}

func dirCompare(dir1, dir2, path string, info os.FileInfo, seen map[string]bool) error {
	if path == dir1 {
		return nil
	}

	rel := path[len(dir1)+1:]
	seen[rel] = true
	if info.IsDir() {
		return nil
	}

	rf := filepath.Join(dir2, rel)
	if _, err := os.Stat(rf); os.IsNotExist(err) {
		return err
	}

	bs, err := ioutil.ReadFile(rf)
	if err != nil {
		return err
	}

	if bytes.Compare(contents, bs) != 0 {
		return fmt.Errorf("bytes for file %s not as expected", rel)
	}
	return nil
}

func runServer(ctx context.Context, adbTurbo, adbPort, server string) error {
	s := "localhost:" + adbPort
	_, err := testutils.ExecOnDevice(
		ctx, adbTurbo, s, "push", []string{server, "/data/local/tmp/server"})
	if err != nil {
		return err
	}

	_, err = testutils.ExecOnDevice(
		ctx, adbTurbo, s, "shell", []string{"chmod", "+x", "/data/local/tmp/server"})
	if err != nil {
		return err
	}
	go func() {
		testutils.ExecOnDevice(
			ctx, adbTurbo, s, "shell", []string{"/data/local/tmp/server"})
	}()
	return nil
}

// TestConnection tests that the bytes between device and host are sent/received correctly
func TestConnection(t *testing.T) {
	adbServerPort, adbPort, emuPort, err := testutils.GetAdbPorts()
	if err != nil {
		t.Fatal(err)
	}

	l := filepath.Join(runfiles, *launcher)
	a := filepath.Join(runfiles, *adbTurbo)
	svr := filepath.Join(runfiles, *server)

	emuDir, err := testutils.SetupEmu(l, adbServerPort, adbPort, emuPort)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(emuDir)
	defer testutils.KillEmu(l, adbServerPort, adbPort, emuPort)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := runServer(ctx, a, adbPort, svr); err != nil {
		t.Fatal(err)
	}

	lis, err := testutils.OpenSocket(filepath.Join(emuDir, emuWorkingDir), qemu.SocketName)
	if err != nil {
		t.Fatalf("error opening socket: %v", err)
	}
	defer lis.Close()

	// Test a few parallel connections
	eg, ctx := errgroup.WithContext(ctx)
	for i := 0; i < 16; i++ {
		eg.Go(func() error {
			qconn, err := qemu.MakeConn(lis)
			if err != nil {
				return err
			}
			defer qconn.Close()

			opts := []grpc.DialOption{grpc.WithInsecure()}
			opts = append(opts, grpc.WithDialer(
				func(addr string, d time.Duration) (net.Conn, error) {
					return qconn, nil
				}))

			conn, err := grpc.Dial("", opts...)
			if err != nil {
				return err
			}
			defer conn.Close()

			k := waterfall_grpc.NewWaterfallClient(conn)
			sent := testBytes(64 * 1024 * 1024)
			rec, err := Echo(ctx, k, sent)

			if err != nil {
				return err
			}

			if bytes.Compare(sent, rec) != 0 {
				return errors.New("bytes received != bytes sent")
			}
			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		t.Fatalf("failed with error: %v", err)
	}
}

func TestPushPull(t *testing.T) {
	adbServerPort, adbPort, emuPort, err := testutils.GetAdbPorts()
	if err != nil {
		t.Fatal(err)
	}

	l := filepath.Join(runfiles, *launcher)
	a := filepath.Join(runfiles, *adbTurbo)
	svr := filepath.Join(runfiles, *server)

	emuDir, err := testutils.SetupEmu(l, adbServerPort, adbPort, emuPort)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(emuDir)
	defer testutils.KillEmu(l, adbServerPort, adbPort, emuPort)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := runServer(ctx, a, adbPort, svr); err != nil {
		t.Fatal(err)
	}

	lis, err := testutils.OpenSocket(filepath.Join(emuDir, emuWorkingDir), qemu.SocketName)
	if err != nil {
		t.Fatalf("error opening socket: %v", err)
	}
	defer lis.Close()

	qconn, err := qemu.MakeConn(lis)
	if err != nil {
		t.Fatal(err)
	}
	defer qconn.Close()

	opts := []grpc.DialOption{grpc.WithInsecure()}
	opts = append(opts, grpc.WithDialer(func(addr string, d time.Duration) (net.Conn, error) {
		return qconn, nil
	}))
	conn, err := grpc.Dial("", opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	td, err := ioutil.TempDir("", "push")
	if err != nil {
		t.Fatal(err)
	}

	pushed, err := makeFs(td, testDir)
	if err != nil {
		t.Fatal(err)
	}

	k := waterfall_grpc.NewWaterfallClient(conn)
	deviceDir := "/data/local/tmp/pushpulltest"
	if err := Push(ctx, k, filepath.Join(td, testDir.path), deviceDir); err != nil {
		t.Fatalf("failed push: %v", err)
	}

	pd, err := ioutil.TempDir("", "pull")
	if err != nil {
		t.Fatal(err)
	}

	if err := Pull(ctx, k, filepath.Join(deviceDir, testDir.path), pd); err != nil {
		t.Fatalf("failed pull: %v", err)
	}

	seen := make(map[string]bool)
	err = filepath.Walk(td, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return dirCompare(td, pd, path, info, seen)
	})

	if err != nil {
		t.Errorf("pushed != pulled: %v", err)
	}

	if len(seen) != len(pushed) {
		t.Errorf("wrong number of files. Got %v expected %v", seen, pushed)
	}

	for _, f := range pushed {
		if !seen[f] {
			t.Errorf("file %s not in tared files", f)
		}
	}
}
