//go:build darwin && !ios

package clipboard

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/ebitengine/purego"
)

const (
	appKitPath  = "/System/Library/Frameworks/AppKit.framework/AppKit"
	libObjCPath = "/usr/lib/libobjc.A.dylib"
)

// appKit holds the Objective-C runtime entry points, classes, selectors and
// the NSPasteboardTypePNG constant ReadImage needs. They are resolved once,
// by loadAppKit, on the first ReadImage call; every later call reuses them.
var appKit struct {
	once sync.Once
	err  error
	// loaded is set once loadAppKit has run, for the startup guard test.
	loaded atomic.Bool

	// objc_msgSend, registered once per signature this package sends.
	sendID    func(self, sel uintptr) uintptr
	sendIDArg func(self, sel, arg uintptr) uintptr
	sendU64   func(self, sel uintptr) uint64
	sendPtr   func(self, sel uintptr) unsafe.Pointer

	pasteboardClass uintptr // NSPasteboard
	poolClass       uintptr // NSAutoreleasePool
	typePNG         uintptr // NSPasteboardTypePNG (an NSString)

	selGeneralPasteboard, selDataForType, selLength, selBytes, selNew, selDrain uintptr
}

// loadAppKit opens AppKit and the Objective-C runtime and resolves what
// ReadImage uses.
func loadAppKit() error {
	appKit.loaded.Store(true)
	objc, err := purego.Dlopen(libObjCPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return fmt.Errorf("load objc runtime: %w", err)
	}
	kit, err := purego.Dlopen(appKitPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return fmt.Errorf("load AppKit: %w", err)
	}
	msgSend, err := purego.Dlsym(objc, "objc_msgSend")
	if err != nil {
		return fmt.Errorf("resolve objc_msgSend: %w", err)
	}
	purego.RegisterFunc(&appKit.sendID, msgSend)
	purego.RegisterFunc(&appKit.sendIDArg, msgSend)
	purego.RegisterFunc(&appKit.sendU64, msgSend)
	purego.RegisterFunc(&appKit.sendPtr, msgSend)

	var getClass func(name string) uintptr
	var registerName func(name string) uintptr
	purego.RegisterLibFunc(&getClass, objc, "objc_getClass")
	purego.RegisterLibFunc(&registerName, objc, "sel_registerName")

	appKit.pasteboardClass = getClass("NSPasteboard")
	appKit.poolClass = getClass("NSAutoreleasePool")
	if appKit.pasteboardClass == 0 || appKit.poolClass == 0 {
		return fmt.Errorf("AppKit classes not found")
	}
	appKit.selGeneralPasteboard = registerName("generalPasteboard")
	appKit.selDataForType = registerName("dataForType:")
	appKit.selLength = registerName("length")
	appKit.selBytes = registerName("bytes")
	appKit.selNew = registerName("new")
	appKit.selDrain = registerName("drain")

	// NSPasteboardTypePNG is an exported variable holding an NSString
	// pointer; Dlsym returns the variable's address. Reading through
	// &sym keeps the conversion to a pointer-to-pointer, which vet accepts,
	// instead of turning the uintptr itself into a pointer.
	sym, err := purego.Dlsym(kit, "NSPasteboardTypePNG")
	if err != nil {
		return fmt.Errorf("resolve NSPasteboardTypePNG: %w", err)
	}
	appKit.typePNG = **(**uintptr)(unsafe.Pointer(&sym))
	return nil
}

// ReadImage returns the PNG image on the general pasteboard. It returns
// ErrUnavailable when the clipboard holds no PNG data. The first call loads
// AppKit (a few milliseconds); later calls do not.
func ReadImage() ([]byte, error) {
	appKit.once.Do(func() { appKit.err = loadAppKit() })
	if appKit.err != nil {
		return nil, appKit.err
	}

	// Cocoa objects returned below are autoreleased; this goroutine's OS
	// thread has no pool of its own, so one is pushed for the duration of
	// the read and the thread is pinned until it is drained.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pool := appKit.sendID(appKit.poolClass, appKit.selNew)
	defer appKit.sendID(pool, appKit.selDrain)

	pasteboard := appKit.sendID(appKit.pasteboardClass, appKit.selGeneralPasteboard)
	if pasteboard == 0 {
		return nil, ErrUnavailable
	}
	return readPNG(pasteboard)
}

// readPNG copies the PNG data on pasteboard (an NSPasteboard) into a Go
// slice. The caller holds an autorelease pool and a locked OS thread.
func readPNG(pasteboard uintptr) ([]byte, error) {
	data := appKit.sendIDArg(pasteboard, appKit.selDataForType, appKit.typePNG)
	if data == 0 {
		return nil, ErrUnavailable
	}
	length := appKit.sendU64(data, appKit.selLength)
	if length == 0 {
		return nil, ErrUnavailable
	}
	bytes := appKit.sendPtr(data, appKit.selBytes)
	if bytes == nil {
		return nil, ErrUnavailable
	}
	out := make([]byte, length)
	copy(out, unsafe.Slice((*byte)(bytes), length))
	return out, nil
}
