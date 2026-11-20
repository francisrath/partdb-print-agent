#pragma once

// IOBluetooth side of pdb-print-ble's RFCOMM and scan-classic modes. Every call hops to the main queue,
// where IOBluetooth delivers its delegate callbacks; btRunMain must be running on the main thread.

// btOpen outcomes, passed to goOpenDone.
enum { BT_OK, BT_UNREACHABLE, BT_NO_SPP, BT_BAD_ADDRESS };

void btRunMain(void);  // runs the main run loop until btStopMain
void btStopMain(void);
int btPowered(void);   // 1 if a Bluetooth controller is on
char *btPaired(void);  // JSON [{"address","name","class"}] of the paired devices; free it
void btOpen(const char *address, int channel); // channel 0 = SDP lookup; ends in goOpenDone
int btWrite(void *p, int n);                   // IOReturn; splits at the channel MTU
void btClose(void);                            // closes the channel (goClosed follows) and the connection
void btInquiryStart(int seconds);              // ends in goInquiryDone
char *btInquiryStop(void);                     // stops the inquiry; JSON of what it found; free it
