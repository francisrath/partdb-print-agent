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
static int gStop;     // main queue only
static BOOL gNoRetry; // set by btClose: a pending btRetryOpen must not reopen
static NSTimer *gSDPPoll;   // the SDP query's completion check (see startSDP)
static NSDate *gSDPBefore;  // the device's last services update when the query started
static BOOL gSDPDone;

// sppChannel finds the serial port service's RFCOMM channel in gDev's (cached) SDP records.
static BOOL sppChannel(BluetoothRFCOMMChannelID *id) {
	IOBluetoothSDPUUID *spp = [IOBluetoothSDPUUID uuid16:kBluetoothSDPUUID16ServiceClassSerialPort];
	IOBluetoothSDPServiceRecord *rec = [gDev getServiceRecordForUUID:spp];
	return rec != nil && [rec getRFCOMMChannelID:id] == kIOReturnSuccess;
}

static BluetoothRFCOMMChannelID gChanID; // for btRetryOpen
static BOOL gInOpen; // inside openRFCOMMChannelAsync, before gChan is set (a completion then is this attempt's)
static IOBluetoothRFCOMMChannel *gStale; // the channel btRetryOpen abandoned (there is at most one retry)

static void openChannel(BluetoothRFCOMMChannelID id) {
	gChanID = id;
	goOpening();
	IOBluetoothRFCOMMChannel *ch = nil;
	gInOpen = YES;
	IOReturn r = [gDev openRFCOMMChannelAsync:&ch withChannelID:id delegate:gDel];
	gInOpen = NO;
	gChan = ch;
	if (r != kIOReturnSuccess) goOpenDone(BT_UNREACHABLE, r, 0);
}

// sdpDone ends the SDP query wait once: open the SPP channel, or report why not.
static void sdpDone(IOReturn status) {
	if (gSDPDone) return;
	gSDPDone = YES;
	[gSDPPoll invalidate];
	gSDPPoll = nil;
	BluetoothRFCOMMChannelID id;
	if (sppChannel(&id)) {
		openChannel(id);
	} else if (status != kIOReturnSuccess) {
		goOpenDone(BT_UNREACHABLE, status, 0);
	} else {
		goOpenDone(BT_NO_SPP, 0, 0);
	}
}

// startSDP queries the device's services. IOBluetooth may fill its record cache without ever calling
// sdpQueryComplete:status: (seen on macOS 27 with a first connect), so the cache is also polled every
// 500 ms. After 10 s with no SPP record: services updated means no SPP; nothing at all means fall back to
// channel 1 (BlueZ's default, and what most SPP printers use).
static void startSDP(void) {
	gSDPDone = NO;
	gSDPBefore = [gDev getLastServicesUpdate];
	IOReturn r = [gDev performSDPQuery:gDel]; // may end in sdpQueryComplete:status:
	if (r != kIOReturnSuccess) {
		sdpDone(r);
		return;
	}
	NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:10];
	gSDPPoll = [NSTimer scheduledTimerWithTimeInterval:0.5 repeats:YES block:^(NSTimer *t) {
		BluetoothRFCOMMChannelID id;
		if (sppChannel(&id)) {
			sdpDone(kIOReturnSuccess);
			return;
		}
		if ([deadline timeIntervalSinceNow] > 0) return;
		NSDate *now = [gDev getLastServicesUpdate];
		if (now != nil && ![now isEqualToDate:gSDPBefore]) {
			sdpDone(kIOReturnSuccess); // updated, but no SPP
			return;
		}
		gSDPDone = YES;
		[gSDPPoll invalidate];
		gSDPPoll = nil;
		fprintf(stderr, "pdb-print-ble: no answer to the service query within 10 s, trying RFCOMM channel 1\n");
		openChannel(1);
	}];
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
	// Only the current attempt's channel counts. During btRetryOpen's delay gChan is nil, so a completion
	// still queued from the channel it closed is dropped here too.
	// IOBluetooth documents no ordering for these callbacks, so the abandoned channel is named, not inferred.
	if ((gStale != nil && ch == gStale) || (!gInOpen && ch != gChan)) return;
	goOpenDone(status == kIOReturnSuccess ? BT_OK : BT_UNREACHABLE, status, [ch getMTU]);
}
- (void)rfcommChannelData:(IOBluetoothRFCOMMChannel *)ch data:(void *)data length:(size_t)length {
	goData(data, (int)length);
}
- (void)rfcommChannelClosed:(IOBluetoothRFCOMMChannel *)ch {
	goClosed();
}
- (void)sdpQueryComplete:(IOBluetoothDevice *)dev status:(IOReturn)status {
	sdpDone(status);
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
			startSDP();
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
		gNoRetry = YES;
		[gChan closeChannel];
	});
}

void btCloseConnection(void) {
	dispatch_async(dispatch_get_main_queue(), ^{ [gDev closeConnection]; });
}

int btIsConnected(const char *address) {
	NSString *a = address != NULL ? [NSString stringWithUTF8String:address] : nil;
	__block int on = 0;
	dispatch_sync(dispatch_get_main_queue(), ^{
		IOBluetoothDevice *d = a != nil ? [IOBluetoothDevice deviceWithAddressString:a] : gDev;
		on = d != nil && [d isConnected];
	});
	return on;
}

void btRetryOpen(void) {
	dispatch_async(dispatch_get_main_queue(), ^{
		// The channel may have opened since the Go side's timer fired (its completion then waits in Go's
		// queue): keep it. Only a still pending attempt is replaced.
		if ([gChan isOpen]) return;
		[gChan setDelegate:nil]; // a late completion of the first attempt is not this one's
		gStale = gChan;
		[gChan closeChannel];
		gChan = nil;
		[gDev closeConnection];
		dispatch_after(dispatch_time(DISPATCH_TIME_NOW, NSEC_PER_SEC), dispatch_get_main_queue(), ^{
			if (!gNoRetry) openChannel(gChanID);
		});
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
