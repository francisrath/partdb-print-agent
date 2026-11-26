#import <Foundation/Foundation.h>
#import <IOKit/IOKitLib.h>
#import <IOUSBHost/IOUSBHost.h>
#include <stdlib.h>
#include <string.h>
#include "usb_darwin.h"

@interface PDBDev : NSObject
@property(strong) IOUSBHostInterface *iface;
@property(strong) IOUSBHostPipe *outPipe, *inPipe;
@end
@implementation PDBDev
@end

static char *dupErr(NSString *what, NSError *e) {
	NSString *s = e ? [NSString stringWithFormat:@"%@: %@ (%#x)", what, e.localizedDescription, (unsigned)e.code] : what;
	return strdup(s.UTF8String);
}

static int intProp(io_service_t s, CFStringRef key, int parents) {
	IOOptionBits opt = parents ? kIORegistryIterateRecursively | kIORegistryIterateParents : 0;
	CFTypeRef v = IORegistryEntrySearchCFProperty(s, kIOServicePlane, key, NULL, opt);
	int n = -1;
	if (v && CFGetTypeID(v) == CFNumberGetTypeID()) CFNumberGetValue(v, kCFNumberIntType, &n);
	if (v) CFRelease(v);
	return n;
}

static NSString *strProp(io_service_t s, CFStringRef key) {
	CFTypeRef v = IORegistryEntrySearchCFProperty(s, kIOServicePlane, key, NULL,
		kIORegistryIterateRecursively | kIORegistryIterateParents);
	NSString *out = @"";
	if (v && CFGetTypeID(v) == CFStringGetTypeID()) out = [(__bridge NSString *)v copy];
	if (v) CFRelease(v);
	return out;
}

static int isPrinter(io_service_t s) {
	int proto = intProp(s, CFSTR("bInterfaceProtocol"), 0);
	return intProp(s, CFSTR("bInterfaceClass"), 0) == 7 && (proto == 1 || proto == 2);
}

// deviceID asks the open interface for its IEEE 1284 device ID (printer class GET_DEVICE_ID).
static NSString *deviceID(IOUSBHostInterface *iface) {
	const IOUSBInterfaceDescriptor *d = iface.interfaceDescriptor;
	IOUSBDeviceRequest req = {
		.bmRequestType = 0xa1, .bRequest = 0, .wValue = 0,
		.wIndex = (uint16_t)(d->bInterfaceNumber << 8 | d->bAlternateSetting), .wLength = 1024,
	};
	NSMutableData *buf = [NSMutableData dataWithLength:1024];
	NSUInteger n = 0;
	if (![iface sendDeviceRequest:req data:buf bytesTransferred:&n completionTimeout:2 error:nil] || n < 2) return @"";
	// 2-byte big-endian length, then the ID
	NSString *s = [[NSString alloc] initWithBytes:(const char *)buf.bytes + 2 length:n - 2 encoding:NSISOLatin1StringEncoding];
	return [s stringByTrimmingCharactersInSet:[NSCharacterSet characterSetWithCharactersInString:@"\0 \t\r\n"]] ?: @"";
}

static IOUSBHostInterface *openIface(io_service_t s, NSError **err) {
	return [[IOUSBHostInterface alloc] initWithIOService:s options:IOUSBHostObjectInitOptionsNone queue:nil
	                                               error:err interestHandler:nil];
}

char *usbList(int withIDs) {
	@autoreleasepool {
		NSMutableString *out = [NSMutableString string];
		io_iterator_t it = 0;
		if (IOServiceGetMatchingServices(kIOMainPortDefault, IOServiceMatching("IOUSBHostInterface"), &it) != KERN_SUCCESS)
			return strdup("");
		io_service_t s;
		while ((s = IOIteratorNext(it))) {
			if (isPrinter(s)) {
				uint64_t id = 0;
				IORegistryEntryGetRegistryEntryID(s, &id);
				NSString *ieee = @"";
				if (withIDs) {
					IOUSBHostInterface *iface = openIface(s, nil); // busy (macOS's queue printing): no ID
					if (iface) {
						ieee = deviceID(iface);
						[iface destroy];
					}
				}
				NSString *clean = [[ieee componentsSeparatedByCharactersInSet:[NSCharacterSet characterSetWithCharactersInString:@"\t\n"]] componentsJoinedByString:@" "];
				[out appendFormat:@"%#llx\t%d\t%d\t%@\t%@\n", id, intProp(s, CFSTR("idVendor"), 1),
					intProp(s, CFSTR("idProduct"), 1), strProp(s, CFSTR("USB Serial Number")), clean];
			}
			IOObjectRelease(s);
		}
		IOObjectRelease(it);
		return strdup(out.UTF8String);
	}
}

void *usbOpen(uint64_t entryID, char **err) {
	@autoreleasepool {
		io_service_t s = IOServiceGetMatchingService(kIOMainPortDefault, IORegistryEntryIDMatching(entryID));
		if (!s) {
			*err = strdup("printer not connected (unplugged?)");
			return NULL;
		}
		NSError *e = nil;
		IOUSBHostInterface *iface = openIface(s, &e);
		IOObjectRelease(s);
		if (!iface) {
			*err = dupErr(@"open", e);
			return NULL;
		}
		const IOUSBInterfaceDescriptor *d = iface.interfaceDescriptor;
		if (d->bInterfaceClass != 7 || (d->bInterfaceProtocol != 1 && d->bInterfaceProtocol != 2)) {
			// another program left the interface on its IPP-over-USB setting
			if (![iface selectAlternateSetting:0 error:&e]) {
				[iface destroy];
				*err = dupErr(@"select the printer setting", e);
				return NULL;
			}
			d = iface.interfaceDescriptor;
		}
		PDBDev *dev = [PDBDev new];
		dev.iface = iface;
		const IOUSBEndpointDescriptor *ep = NULL;
		while ((ep = IOUSBGetNextEndpointDescriptor(iface.configurationDescriptor, d, (const IOUSBDescriptorHeader *)ep))) {
			if ((ep->bmAttributes & 3) != 2) continue; // bulk only
			IOUSBHostPipe *p = [iface copyPipeWithAddress:ep->bEndpointAddress error:&e];
			if (ep->bEndpointAddress & 0x80) {
				if (!dev.inPipe) dev.inPipe = p;
			} else if (!dev.outPipe) {
				dev.outPipe = p;
			}
		}
		if (!dev.outPipe) {
			[iface destroy];
			*err = dupErr(@"the printer interface has no bulk OUT endpoint", nil);
			return NULL;
		}
		return (void *)CFBridgingRetain(dev);
	}
}

int usbWrite(void *h, const void *p, int n, char **err) {
	@autoreleasepool {
		PDBDev *dev = (__bridge PDBDev *)h;
		NSMutableData *data = [NSMutableData dataWithBytes:p length:n];
		NSUInteger sent = 0;
		NSError *e = nil;
		if (![dev.outPipe sendIORequestWithData:data bytesTransferred:&sent completionTimeout:30 error:&e]) {
			*err = dupErr([NSString stringWithFormat:@"write (%lu of %d bytes sent)", (unsigned long)sent, n], e);
			return -1;
		}
		return 0;
	}
}

int usbRead(void *h, void *buf, int n, int timeoutMS, char **err) {
	@autoreleasepool {
		PDBDev *dev = (__bridge PDBDev *)h;
		if (!dev.inPipe) {
			usleep(timeoutMS * 1000); // a unidirectional printer never answers
			return 0;
		}
		NSMutableData *data = [NSMutableData dataWithLength:n];
		NSUInteger got = 0;
		NSError *e = nil;
		if (![dev.inPipe sendIORequestWithData:data bytesTransferred:&got completionTimeout:timeoutMS / 1000.0 error:&e]) {
			if (e.code == kIOReturnTimeout || (unsigned)e.code == 0xe0004051u || e.code == kIOReturnAborted) return 0;
			*err = dupErr(@"read", e);
			return -1;
		}
		memcpy(buf, data.bytes, got);
		return (int)got;
	}
}

void usbClose(void *h) {
	@autoreleasepool {
		PDBDev *dev = CFBridgingRelease(h);
		[dev.iface destroy];
	}
}
