//go:build darwin && cgo

#ifndef TELEOP_GENERIC_HID_H
#define TELEOP_GENERIC_HID_H
#include <stdint.h>
#include <stddef.h>
typedef struct {
 uint64_t id;
 uint16_t vendor,product;
 char name[256];
 char transport[32];
} teleop_hid_device;
typedef struct {
 uint32_t cookie,page,usage;
 int kind;
 int64_t minimum,maximum,value;
} teleop_hid_element;
// All arrays are heap-owned by the caller, including empty results.
int teleop_hid_list(teleop_hid_device **devices,size_t *count);
void *teleop_hid_open(uint64_t id,teleop_hid_device *info,teleop_hid_element **elements,size_t *count,int *error);
int teleop_hid_poll(void *handle,int64_t *values,size_t count);
void teleop_hid_close(void *handle);
#endif
