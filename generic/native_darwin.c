//go:build darwin && cgo

#include "native_darwin.h"
#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/IOKitLib.h>
#include <IOKit/hid/IOHIDLib.h>
#include <stdlib.h>
#include <string.h>

typedef struct {
 IOHIDDeviceRef device;
 CFMutableArrayRef elements;
 uint64_t id;
} teleop_hid_handle;

static int64_t number(IOHIDDeviceRef d,CFStringRef key) {
 CFTypeRef value=IOHIDDeviceGetProperty(d,key);
 int64_t n=0;
 if(value && CFGetTypeID(value)==CFNumberGetTypeID()) CFNumberGetValue(value,kCFNumberSInt64Type,&n);
 return n;
}
static void string(IOHIDDeviceRef d,CFStringRef key,char *out,size_t size) {
 CFTypeRef value=IOHIDDeviceGetProperty(d,key);
 out[0]=0;
 if(value && CFGetTypeID(value)==CFStringGetTypeID()) CFStringGetCString(value,out,size,kCFStringEncodingUTF8);
}
static int gamepad(IOHIDDeviceRef d) {
 return IOHIDDeviceConformsTo(d,1,4) || IOHIDDeviceConformsTo(d,1,5);
}
static uint64_t identifier(IOHIDDeviceRef d) {
 uint64_t id=0;
 io_service_t service=IOHIDDeviceGetService(d);
 if(service) IORegistryEntryGetRegistryEntryID(service,&id);
 return id;
}
static void info(IOHIDDeviceRef d,teleop_hid_device *out) {
 memset(out,0,sizeof(*out));
 out->id=identifier(d);
 out->vendor=(uint16_t)number(d,CFSTR(kIOHIDVendorIDKey));
 out->product=(uint16_t)number(d,CFSTR(kIOHIDProductIDKey));
 string(d,CFSTR(kIOHIDProductKey),out->name,sizeof(out->name));
 string(d,CFSTR(kIOHIDTransportKey),out->transport,sizeof(out->transport));
}
// Matching limits enumeration to joystick/gamepad collections. No keyboard,
// mouse, feature report, output report, or exclusive device access is requested.
static IOHIDManagerRef manager(void) {
 IOHIDManagerRef m=IOHIDManagerCreate(kCFAllocatorDefault,kIOHIDOptionsTypeNone);
 if(!m) return NULL;
 CFMutableArrayRef matches=CFArrayCreateMutable(NULL,0,&kCFTypeArrayCallBacks);
 for(int usage=4;usage<=5;usage++) {
  int page=1;
  CFNumberRef p=CFNumberCreate(NULL,kCFNumberIntType,&page),u=CFNumberCreate(NULL,kCFNumberIntType,&usage);
  CFMutableDictionaryRef d=CFDictionaryCreateMutable(NULL,0,&kCFTypeDictionaryKeyCallBacks,&kCFTypeDictionaryValueCallBacks);
  CFDictionarySetValue(d,CFSTR(kIOHIDDeviceUsagePageKey),p);
  CFDictionarySetValue(d,CFSTR(kIOHIDDeviceUsageKey),u);
  CFArrayAppendValue(matches,d);
  CFRelease(d);CFRelease(p);CFRelease(u);
 }
 IOHIDManagerSetDeviceMatchingMultiple(m,matches);
 CFRelease(matches);
 return m;
}
static int status(IOReturn result) {
 if(result==kIOReturnSuccess) return 0;
 if(result==kIOReturnNotPermitted || result==kIOReturnNotPrivileged || result==kIOReturnExclusiveAccess) return -3;
 if(result==kIOReturnNoDevice || result==kIOReturnNotAttached || result==kIOReturnOffline) return -2;
 return -1;
}
int teleop_hid_list(teleop_hid_device **devices,size_t *count) {
 *devices=NULL;*count=0;
 IOHIDManagerRef m=manager();if(!m)return -1;
 CFSetRef set=IOHIDManagerCopyDevices(m);
 if(set) {
  CFIndex n=CFSetGetCount(set);
  IOHIDDeviceRef *all=calloc((size_t)n+1,sizeof(*all));
  teleop_hid_device *out=calloc((size_t)n+1,sizeof(*out));
  if(!all || !out) {free(all);free(out);CFRelease(set);CFRelease(m);return -1;}
  CFSetGetValues(set,(const void **)all);
  for(CFIndex i=0;i<n;i++) if(gamepad(all[i])) { info(all[i],&out[*count]); if(out[*count].id)(*count)++; }
  free(all);*devices=out;CFRelease(set);
 }
 CFRelease(m);return 0;
}
void teleop_hid_close(void *opaque) {
 teleop_hid_handle *h=opaque;if(!h)return;
 if(h->elements)CFRelease(h->elements);
 if(h->device){IOHIDDeviceClose(h->device,kIOHIDOptionsTypeNone);CFRelease(h->device);}
 free(h);
}
void *teleop_hid_open(uint64_t id,teleop_hid_device *out,teleop_hid_element **elements,size_t *count,int *error) {
 *elements=NULL;*count=0;*error=-2;
 IOHIDManagerRef m=manager();if(!m){*error=-1;return NULL;}
 CFSetRef set=IOHIDManagerCopyDevices(m);
 IOHIDDeviceRef found=NULL;
 if(set) {
  CFIndex n=CFSetGetCount(set);
  IOHIDDeviceRef *all=calloc((size_t)n+1,sizeof(*all));
  if(!all){CFRelease(set);CFRelease(m);*error=-1;return NULL;}
  CFSetGetValues(set,(const void **)all);
  for(CFIndex i=0;i<n;i++) if(gamepad(all[i]) && identifier(all[i])==id){found=all[i];CFRetain(found);break;}
  free(all);CFRelease(set);
 }
 CFRelease(m);
 if(!found)return NULL;
 *error=status(IOHIDDeviceOpen(found,kIOHIDOptionsTypeNone));
 if(*error){CFRelease(found);return NULL;}
 teleop_hid_handle *h=calloc(1,sizeof(*h));
 if(!h){IOHIDDeviceClose(found,0);CFRelease(found);*error=-1;return NULL;}
 h->device=found;h->id=id;h->elements=CFArrayCreateMutable(NULL,0,&kCFTypeArrayCallBacks);
 CFArrayRef all=IOHIDDeviceCopyMatchingElements(found,NULL,kIOHIDOptionsTypeNone);
 if(!all || !h->elements){if(all)CFRelease(all);teleop_hid_close(h);*error=-1;return NULL;}
 CFIndex n=CFArrayGetCount(all);
 teleop_hid_element *result=calloc((size_t)n+1,sizeof(*result));
 if(!result){CFRelease(all);teleop_hid_close(h);*error=-1;return NULL;}
 for(CFIndex i=0;i<n;i++) {
  IOHIDElementRef e=(IOHIDElementRef)CFArrayGetValueAtIndex(all,i);
  IOHIDElementType type=IOHIDElementGetType(e);
  if(type<kIOHIDElementTypeInput_Misc || type>kIOHIDElementTypeInput_ScanCodes || IOHIDElementIsRelative(e) || IOHIDElementIsArray(e))continue;
  uint32_t page=IOHIDElementGetUsagePage(e),usage=IOHIDElementGetUsage(e);
  int kind=0;
  if(page==9)kind=1;
  else if(page==1 && usage>=0x30 && usage<=0x36)kind=2;
  else if(page==1 && usage==0x39)kind=3;
  else if(page==1 && usage>=0x90 && usage<=0x93)kind=1;
  if(!kind)continue;
  CFIndex minimum=IOHIDElementGetLogicalMin(e),maximum=IOHIDElementGetLogicalMax(e);
  if(maximum<=minimum)continue;
  result[*count]=(teleop_hid_element){.cookie=(uint32_t)IOHIDElementGetCookie(e),.page=page,.usage=usage,.kind=kind,.minimum=minimum,.maximum=maximum};
  CFArrayAppendValue(h->elements,e);(*count)++;
 }
 CFRelease(all);info(found,out);*elements=result;*error=0;return h;
}
int teleop_hid_poll(void *opaque,int64_t *values,size_t count) {
 teleop_hid_handle *h=opaque;
 if(!h || count!=(size_t)CFArrayGetCount(h->elements))return -1;
 // A new registry lookup affirms that this exact attachment still exists. A
 // cached HID value alone is not evidence of an attached controller.
 io_service_t live=IOServiceGetMatchingService(kIOMainPortDefault,IORegistryEntryIDMatching(h->id));
 if(!live)return -2;
 IOObjectRelease(live);
 for(size_t i=0;i<count;i++) {
  IOHIDElementRef element=(IOHIDElementRef)CFArrayGetValueAtIndex(h->elements,(CFIndex)i);
  IOHIDValueRef value=NULL;
  int result=status(IOHIDDeviceGetValue(h->device,element,&value));
  if(result)return result;
  if(!value)return -1;
  values[i]=IOHIDValueGetIntegerValue(value);
 }
 return 0;
}
