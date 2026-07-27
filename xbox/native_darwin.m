//go:build darwin && cgo

#import <Foundation/Foundation.h>
#import <GameController/GameController.h>
#include <pthread.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include "native_darwin.h"

#define TELEOP_QUEUE_SIZE 4096

typedef struct {
    GCController *controller;
    id disconnect_observer;
    pthread_mutex_t mutex;
    pthread_cond_t condition;
    teleop_gc_state queue[TELEOP_QUEUE_SIZE];
    size_t head;
    size_t count;
    uint64_t sequence;
    int dropped;
    int disconnected;
    int closed;
} teleop_gc_handle;

static teleop_gc_state teleop_gc_capture(teleop_gc_handle *handle, GCExtendedGamepad *gamepad) {
    teleop_gc_state state;
    memset(&state, 0, sizeof(state));
    state.left_x = gamepad.leftThumbstick.xAxis.value;
    state.left_y = gamepad.leftThumbstick.yAxis.value;
    state.right_x = gamepad.rightThumbstick.xAxis.value;
    state.right_y = gamepad.rightThumbstick.yAxis.value;
    state.left_trigger = gamepad.leftTrigger.value;
    state.right_trigger = gamepad.rightTrigger.value;
    state.a = gamepad.buttonA.isPressed;
    state.b = gamepad.buttonB.isPressed;
    state.x = gamepad.buttonX.isPressed;
    state.y = gamepad.buttonY.isPressed;
    state.left_bumper = gamepad.leftShoulder.isPressed;
    state.right_bumper = gamepad.rightShoulder.isPressed;
    if (gamepad.leftThumbstickButton != nil) {
        state.left_stick = gamepad.leftThumbstickButton.isPressed;
    }
    if (gamepad.rightThumbstickButton != nil) {
        state.right_stick = gamepad.rightThumbstickButton.isPressed;
    }
    if (gamepad.buttonMenu != nil) {
        state.menu = gamepad.buttonMenu.isPressed;
    }
    if (gamepad.buttonOptions != nil) {
        state.view = gamepad.buttonOptions.isPressed;
    }
    if (gamepad.buttonHome != nil) {
        state.home = gamepad.buttonHome.isPressed;
    }
    SEL share_selector = NSSelectorFromString(@"buttonShare");
    if ([gamepad respondsToSelector:share_selector]) {
        GCControllerButtonInput *share = [gamepad valueForKey:@"buttonShare"];
        if (share != nil) state.share = share.isPressed;
    }
    state.dpad_up = gamepad.dpad.up.isPressed;
    state.dpad_down = gamepad.dpad.down.isPressed;
    state.dpad_left = gamepad.dpad.left.isPressed;
    state.dpad_right = gamepad.dpad.right.isPressed;
    state.sequence = ++handle->sequence;
    struct timespec now;
    clock_gettime(CLOCK_REALTIME, &now);
    state.timestamp = (double)now.tv_sec + ((double)now.tv_nsec / 1000000000.0);
    return state;
}

static void teleop_gc_enqueue(teleop_gc_handle *handle, GCExtendedGamepad *gamepad) {
    pthread_mutex_lock(&handle->mutex);
    if (!handle->closed) {
        teleop_gc_state state = teleop_gc_capture(handle, gamepad);
        if (handle->count == TELEOP_QUEUE_SIZE) {
            handle->head = (handle->head + 1) % TELEOP_QUEUE_SIZE;
            handle->count--;
            handle->dropped = 1;
        }
        size_t tail = (handle->head + handle->count) % TELEOP_QUEUE_SIZE;
        handle->queue[tail] = state;
        handle->count++;
        pthread_cond_signal(&handle->condition);
    }
    pthread_mutex_unlock(&handle->mutex);
}

int teleop_gc_count(void) {
    @autoreleasepool {
        [GCController startWirelessControllerDiscoveryWithCompletionHandler:^{}];
        return (int)[[GCController controllers] count];
    }
}

int teleop_gc_info(int index, char *name, size_t name_size, uint32_t *features) {
    @autoreleasepool {
        NSArray<GCController *> *controllers = [GCController controllers];
        if (index < 0 || index >= (int)[controllers count]) {
            return 0;
        }
        GCController *controller = controllers[(NSUInteger)index];
        GCExtendedGamepad *gamepad = controller.extendedGamepad;
        if (gamepad == nil) {
            return 0;
        }
        NSString *vendor = controller.vendorName ?: @"Game controller";
        if (name != NULL && name_size > 0) {
            const char *value = [vendor UTF8String];
            strncpy(name, value, name_size - 1);
            name[name_size - 1] = '\0';
        }
        uint32_t result = 0;
        if (gamepad.buttonMenu != nil) result |= 1;
        if (gamepad.buttonOptions != nil) result |= 2;
        if (gamepad.buttonHome != nil) result |= 4;
        SEL share_selector = NSSelectorFromString(@"buttonShare");
        if ([gamepad respondsToSelector:share_selector] && [gamepad valueForKey:@"buttonShare"] != nil) result |= 8;
        if (gamepad.leftThumbstickButton != nil) result |= 16;
        if (gamepad.rightThumbstickButton != nil) result |= 32;
        if (features != NULL) *features = result;
        return 1;
    }
}

void *teleop_gc_open(int index) {
    @autoreleasepool {
        NSArray<GCController *> *controllers = [GCController controllers];
        if (index < 0 || index >= (int)[controllers count]) {
            return NULL;
        }
        GCController *controller = controllers[(NSUInteger)index];
        GCExtendedGamepad *gamepad = controller.extendedGamepad;
        if (gamepad == nil) {
            return NULL;
        }
        teleop_gc_handle *handle = calloc(1, sizeof(teleop_gc_handle));
        if (handle == NULL) {
            return NULL;
        }
        handle->controller = [controller retain];
        pthread_mutex_init(&handle->mutex, NULL);
        pthread_cond_init(&handle->condition, NULL);

        gamepad.valueChangedHandler = ^(GCExtendedGamepad *changed, GCControllerElement *element) {
            (void)element;
            teleop_gc_enqueue(handle, changed);
        };
        handle->disconnect_observer = [[NSNotificationCenter defaultCenter]
            addObserverForName:GCControllerDidDisconnectNotification
            object:controller
            queue:nil
            usingBlock:^(NSNotification *note) {
                (void)note;
                pthread_mutex_lock(&handle->mutex);
                handle->disconnected = 1;
                pthread_cond_broadcast(&handle->condition);
                pthread_mutex_unlock(&handle->mutex);
            }];
        teleop_gc_enqueue(handle, gamepad);
        return handle;
    }
}

int teleop_gc_next(void *opaque, teleop_gc_state *state, int timeout_ms) {
    teleop_gc_handle *handle = (teleop_gc_handle *)opaque;
    if (handle == NULL || state == NULL) return -1;

    struct timespec deadline;
    clock_gettime(CLOCK_REALTIME, &deadline);
    deadline.tv_sec += timeout_ms / 1000;
    deadline.tv_nsec += (timeout_ms % 1000) * 1000000L;
    if (deadline.tv_nsec >= 1000000000L) {
        deadline.tv_sec++;
        deadline.tv_nsec -= 1000000000L;
    }

    pthread_mutex_lock(&handle->mutex);
    while (handle->count == 0 && !handle->closed && !handle->disconnected) {
        int result = pthread_cond_timedwait(&handle->condition, &handle->mutex, &deadline);
        if (result != 0) {
            pthread_mutex_unlock(&handle->mutex);
            return 0;
        }
    }
    if (handle->closed) {
        pthread_mutex_unlock(&handle->mutex);
        return -1;
    }
    if (handle->disconnected) {
        pthread_mutex_unlock(&handle->mutex);
        return -2;
    }
    *state = handle->queue[handle->head];
    handle->head = (handle->head + 1) % TELEOP_QUEUE_SIZE;
    handle->count--;
    if (handle->dropped) {
        state->dropped = 1;
        handle->dropped = 0;
    }
    pthread_mutex_unlock(&handle->mutex);
    return 1;
}

void teleop_gc_close(void *opaque) {
    teleop_gc_handle *handle = (teleop_gc_handle *)opaque;
    if (handle == NULL) return;
    @autoreleasepool {
        handle->controller.extendedGamepad.valueChangedHandler = nil;
        if (handle->disconnect_observer != nil) {
            [[NSNotificationCenter defaultCenter] removeObserver:handle->disconnect_observer];
            handle->disconnect_observer = nil;
        }
        pthread_mutex_lock(&handle->mutex);
        handle->closed = 1;
        pthread_cond_broadcast(&handle->condition);
        pthread_mutex_unlock(&handle->mutex);
        [handle->controller release];
        handle->controller = nil;
        // The handle remains allocated so a concurrent timed wait can safely
        // observe closure. Controller handles are few and process-scoped.
    }
}
