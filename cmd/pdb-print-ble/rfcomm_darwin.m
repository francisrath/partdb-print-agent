//go:build darwin && cgo

#import <Foundation/Foundation.h>
#import <IOBluetooth/IOBluetooth.h>
#include "rfcomm_darwin.h"
#include "_cgo_export.h"

@interface PDBDelegate : NSObject <IOBluetoothRFCOMMChannelDelegate, IOBluetoothDeviceInquiryDelegate>
@end

static PDBDelegate *gDel;
static IOBluetoothDevice *gDev;
static IOBluetoothRFCOMMChannel *gChan;
static IOBluetoothDeviceInquiry *gInq;
static volatile int gStop;

// sppChannel finds the serial port service's RFCOMM channel in gDev's (cached) SDP records.
static BOOL sppChannel(BluetoothRFCOMMChannelID *id) {
	IOBluetoothSDPUUID *spp = [IOBluetoothSDPUUID uuid16:kBluetoothSDPUUID16ServiceClassSerialPort];
	IOBluetoothSDPServiceRecord *rec = [gDev getServiceRecordForUUID:spp];
	return rec != nil && [rec getRFCOMMChannelID:id] == kIOReturnSuccess;
}

static void openChannel(BluetoothRFCOMMChannelID id) {
	IOBluetoothRFCOMMChannel *ch = nil;
	IOReturn r = [gDev openRFCOMMChannelAsync:&ch withChannelID:id delegate:gDel];
	gChan = ch;
	if (r != kIOReturnSuccess) goOpenDone(BT_UNREACHABLE, r, 0);
}

static char *devicesJSON(NSArray *devs) {
	NSMutableArray *a = [NSMutableArray array];
	for (IOBluetoothDevice *d in devs) {
		[a addObject:@{@"address": [d addressString] ?: @"", @"name": [d name] ?: @"", @"class": @([d classOfDevice])}];
	}
	NSData *j = [NSJSONSerialization dataWithJSONObject:a options:0 error:nil];
	char *s = calloc([j length] + 1, 1);
	memcpy(s, [j bytes], [j length]);
	return s;
}

@implementation PDBDelegate
- (void)rfcommChannelOpenComplete:(IOBluetoothRFCOMMChannel *)ch status:(IOReturn)status {
	goOpenDone(status == kIOReturnSuccess ? BT_OK : BT_UNREACHABLE, status, [ch getMTU]);
}
- (void)rfcommChannelData:(IOBluetoothRFCOMMChannel *)ch data:(void *)data length:(size_t)length {
	goData(data, (int)length);
}
- (void)rfcommChannelClosed:(IOBluetoothRFCOMMChannel *)ch {
	goClosed();
}
- (void)sdpQueryComplete:(IOBluetoothDevice *)dev status:(IOReturn)status {
	BluetoothRFCOMMChannelID id;
	if (status != kIOReturnSuccess) {
		goOpenDone(BT_UNREACHABLE, status, 0);
	} else if (sppChannel(&id)) {
		openChannel(id);
	} else {
		goOpenDone(BT_NO_SPP, 0, 0);
	}
}
- (void)deviceInquiryComplete:(IOBluetoothDeviceInquiry *)sender error:(IOReturn)error aborted:(BOOL)aborted {
	goInquiryDone(error);
}
@end

void btRunMain(void) {
	// a timer keeps the loop from returning at once while it has nothing else to wait for
	CFRunLoopTimerRef keep = CFRunLoopTimerCreate(NULL, CFAbsoluteTimeGetCurrent() + 3600, 3600, 0, 0, NULL, NULL);
	CFRunLoopAddTimer(CFRunLoopGetMain(), keep, kCFRunLoopDefaultMode);
	while (!gStop) CFRunLoopRunInMode(kCFRunLoopDefaultMode, 0.5, false);
	CFRunLoopTimerInvalidate(keep);
	CFRelease(keep);
}

void btStopMain(void) {
	dispatch_async(dispatch_get_main_queue(), ^{
		gStop = 1;
		CFRunLoopStop(CFRunLoopGetMain());
	});
}

int btPowered(void) {
	__block int on = 0;
	dispatch_sync(dispatch_get_main_queue(), ^{
		IOBluetoothHostController *c = [IOBluetoothHostController defaultController];
		on = c != nil && [c powerState] == kBluetoothHCIPowerStateON;
	});
	return on;
}

char *btPaired(void) {
	__block char *s;
	dispatch_sync(dispatch_get_main_queue(), ^{ s = devicesJSON([IOBluetoothDevice pairedDevices]); });
	return s;
}

void btOpen(const char *address, int channel) {
	NSString *a = [NSString stringWithUTF8String:address];
	dispatch_async(dispatch_get_main_queue(), ^{
		if (gDel == nil) gDel = [PDBDelegate new];
		gDev = [IOBluetoothDevice deviceWithAddressString:a];
		BluetoothRFCOMMChannelID id;
		if (gDev == nil) {
			goOpenDone(BT_BAD_ADDRESS, 0, 0);
		} else if (channel > 0) {
			openChannel((BluetoothRFCOMMChannelID)channel);
		} else if (sppChannel(&id)) { // remembered from pairing
			openChannel(id);
		} else {
			IOReturn r = [gDev performSDPQuery:gDel]; // ends in sdpQueryComplete:status:
			if (r != kIOReturnSuccess) goOpenDone(BT_UNREACHABLE, r, 0);
		}
	});
}

int btWrite(void *p, int n) {
	__block IOReturn r = kIOReturnNotOpen;
	dispatch_sync(dispatch_get_main_queue(), ^{
		if (gChan == nil) return;
		int mtu = [gChan getMTU];
		if (mtu <= 0) mtu = 127;
		r = kIOReturnSuccess;
		for (int i = 0; i < n && r == kIOReturnSuccess; i += mtu) {
			r = [gChan writeSync:(char *)p + i length:(UInt16)MIN(mtu, n - i)];
		}
	});
	return r;
}

void btClose(void) {
	dispatch_async(dispatch_get_main_queue(), ^{
		[gChan closeChannel];
		[gDev closeConnection];
	});
}

void btInquiryStart(int seconds) {
	dispatch_async(dispatch_get_main_queue(), ^{
		if (gDel == nil) gDel = [PDBDelegate new];
		gInq = [IOBluetoothDeviceInquiry inquiryWithDelegate:gDel];
		[gInq setInquiryLength:(uint8_t)seconds];
		IOReturn r = [gInq start];
		if (r != kIOReturnSuccess) goInquiryDone(r);
	});
}

char *btInquiryStop(void) {
	__block char *s;
	dispatch_sync(dispatch_get_main_queue(), ^{
		[gInq stop];
		s = devicesJSON([gInq foundDevices]);
	});
	return s;
}
