//go:build darwin && !ios

package clipboard

import (
	"errors"
	"runtime"
	"testing"
	"unsafe"

	"github.com/ebitengine/purego"
)

// Importing the package must not load AppKit: that is its reason to exist.
// This test must run before any test that calls ReadImage.
func TestAppKitNotLoadedAtInit(t *testing.T) {
	if appKit.loaded.Load() {
		t.Fatal("AppKit was loaded before the first ReadImage call")
	}
}

// ReadImage loads AppKit and returns either PNG bytes or ErrUnavailable,
// whatever the clipboard holds on the machine running the test. It never
// writes the clipboard.
func TestReadImageLoadsAppKitLazily(t *testing.T) {
	data, err := ReadImage()
	if !appKit.loaded.Load() {
		t.Fatal("ReadImage did not load AppKit")
	}
	if appKit.err != nil {
		t.Fatalf("loading AppKit failed: %v", appKit.err)
	}
	switch {
	case err == nil && len(data) < 8:
		t.Fatalf("ReadImage returned %d bytes and no error", len(data))
	case err != nil && !errors.Is(err, ErrUnavailable):
		t.Fatalf("ReadImage error = %v, want ErrUnavailable or PNG data", err)
	case err == nil && string(data[1:4]) != "PNG":
		t.Fatalf("ReadImage returned non-PNG data (% x)", data[:8])
	}
}

// readPNG returns exactly the bytes put on a pasteboard as PNG. The test
// uses a private, uniquely named pasteboard, so the user's clipboard is
// never read or changed.
func TestReadPNGFromPrivatePasteboard(t *testing.T) {
	if _, err := ReadImage(); err != nil && !errors.Is(err, ErrUnavailable) {
		t.Fatalf("load AppKit: %v", err)
	}
	var sendSetData func(self, sel, data, typ uintptr) bool
	var sendData func(self, sel uintptr, bytes unsafe.Pointer, length uint64) uintptr
	msgSend := objcMsgSendForTest(t)
	purego.RegisterFunc(&sendSetData, msgSend)
	purego.RegisterFunc(&sendData, msgSend)

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pool := appKit.sendID(appKit.poolClass, appKit.selNew)
	defer appKit.sendID(pool, appKit.selDrain)

	pb := appKit.sendID(appKit.pasteboardClass, registerNameForTest(t, "pasteboardWithUniqueName"))
	if pb == 0 {
		t.Fatal("could not create a private pasteboard")
	}
	defer appKit.sendID(pb, registerNameForTest(t, "releaseGlobally"))
	appKit.sendU64(pb, registerNameForTest(t, "clearContents"))

	if _, err := readPNG(pb); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("empty pasteboard: err = %v, want ErrUnavailable", err)
	}

	png := []byte("\x89PNG\r\n\x1a\n-not-a-real-image-but-bytes-are-bytes")
	nsData := sendData(classForTest(t, "NSData"), registerNameForTest(t, "dataWithBytes:length:"), unsafe.Pointer(&png[0]), uint64(len(png)))
	if !sendSetData(pb, registerNameForTest(t, "setData:forType:"), nsData, appKit.typePNG) {
		t.Fatal("setData:forType: failed")
	}
	got, err := readPNG(pb)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(png) {
		t.Fatalf("readPNG = %q, want %q", got, png)
	}
}

func objcMsgSendForTest(t *testing.T) uintptr {
	t.Helper()
	lib, err := purego.Dlopen(libObjCPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		t.Fatal(err)
	}
	fn, err := purego.Dlsym(lib, "objc_msgSend")
	if err != nil {
		t.Fatal(err)
	}
	return fn
}

func registerNameForTest(t *testing.T, name string) uintptr {
	t.Helper()
	lib, err := purego.Dlopen(libObjCPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		t.Fatal(err)
	}
	var registerName func(string) uintptr
	purego.RegisterLibFunc(&registerName, lib, "sel_registerName")
	return registerName(name)
}

func classForTest(t *testing.T, name string) uintptr {
	t.Helper()
	lib, err := purego.Dlopen(libObjCPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		t.Fatal(err)
	}
	var getClass func(string) uintptr
	purego.RegisterLibFunc(&getClass, lib, "objc_getClass")
	return getClass(name)
}
