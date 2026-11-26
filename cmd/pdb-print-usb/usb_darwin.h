#include <stdint.h>

// Lines "device\tvid\tpid\tserial\tieee1284\n" for every USB printer with a raster printer-class interface
// (class 7, protocol 1 or 2); device is the USB device's registry entry id; the IEEE 1284 ID only when
// withIDs. Free with free().
char *usbList(int withIDs);
// Opens the raster printer interface of the USB device with that registry entry id; NULL with *err set (free it) when it can't.
void *usbOpen(uint64_t entryID, char **err);
// Returns 0, or -1 with *err set.
int usbWrite(void *h, const void *p, int n, char **err);
// Returns the bytes read (0 when nothing came within timeoutMS), or -1 with *err set. Only when usbCanRead.
int usbRead(void *h, void *buf, int n, int timeoutMS, char **err);
// Whether the printer can answer (has a bulk IN endpoint).
int usbCanRead(void *h);
void usbClose(void *h);
