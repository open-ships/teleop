//go:build darwin && cgo

#import <Foundation/Foundation.h>
#import <GameController/GameController.h>
#import <objc/runtime.h>
#include <pthread.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include "native_darwin.h"

#define TELEOP_QUEUE_SIZE 4096

typedef struct teleop_gc_handle {
    GCController *controller;
    id disconnect_observer;
    dispatch_queue_t handler_queue;
    dispatch_queue_t previous_handler_queue;
    pthread_mutex_t mutex;
    pthread_cond_t condition;
    teleop_gc_state queue[TELEOP_QUEUE_SIZE];
    size_t head;
    size_t count;
    uint64_t sequence;
    int dropped;
    int disconnected;
    int closed;
    uint64_t generation;
    size_t active_callbacks;
    struct teleop_gc_handle *next;
} teleop_gc_handle;

static pthread_once_t teleop_gc_discovery_once = PTHREAD_ONCE_INIT;
static char teleop_gc_handler_queue_key;
static char teleop_gc_identifier_key;
static pthread_mutex_t teleop_gc_identifier_mutex = PTHREAD_MUTEX_INITIALIZER;
static uint64_t teleop_gc_next_identifier;
static pthread_mutex_t teleop_gc_registry_mutex = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t teleop_gc_registry_condition = PTHREAD_COND_INITIALIZER;
static teleop_gc_handle *teleop_gc_active_handles;
static uint64_t teleop_gc_next_generation;

static void teleop_gc_start_discovery(void) {
    [GCController startWirelessControllerDiscoveryWithCompletionHandler:^{}];
}

static NSArray<GCController *> *teleop_gc_controllers(void) {
    pthread_once(&teleop_gc_discovery_once, teleop_gc_start_discovery);

    // GameController populates its registry through the calling thread's run
    // loop. Command-line programs do not otherwise run one before discovery,
    // so an immediate call to +controllers incorrectly appears empty. Pump a
    // short slice even when populated so hotplug changes can be delivered.
    for (int attempt = 0; attempt < 20; attempt++) {
        [[NSRunLoop currentRunLoop]
            runUntilDate:[NSDate dateWithTimeIntervalSinceNow:0.025]];
        NSArray<GCController *> *controllers = [GCController controllers];
        if ([controllers count] > 0) {
            return controllers;
        }
    }
    return [GCController controllers];
}

static void teleop_gc_copy_string(NSString *value, char *destination, size_t size) {
    if (destination == NULL || size == 0) {
        return;
    }
    const char *utf8 = [value UTF8String];
    if (utf8 == NULL) {
        destination[0] = '\0';
        return;
    }
    strncpy(destination, utf8, size - 1);
    destination[size - 1] = '\0';
}

static uint64_t teleop_gc_identifier(GCController *controller) {
    pthread_mutex_lock(&teleop_gc_identifier_mutex);
    NSNumber *stored = objc_getAssociatedObject(
        controller,
        &teleop_gc_identifier_key
    );
    if (stored == nil) {
        stored = [NSNumber numberWithUnsignedLongLong:++teleop_gc_next_identifier];
        objc_setAssociatedObject(
            controller,
            &teleop_gc_identifier_key,
            stored,
            OBJC_ASSOCIATION_RETAIN_NONATOMIC
        );
    }
    uint64_t identifier = [stored unsignedLongLongValue];
    pthread_mutex_unlock(&teleop_gc_identifier_mutex);
    return identifier;
}

static GCController *teleop_gc_controller_by_id(uint64_t identifier) {
    for (GCController *controller in [GCController controllers]) {
        if (teleop_gc_identifier(controller) == identifier) {
            return controller;
        }
    }
    return nil;
}

static int teleop_gc_acquire_callback(teleop_gc_handle *handle, uint64_t generation) {
    int found = 0;
    pthread_mutex_lock(&teleop_gc_registry_mutex);
    for (teleop_gc_handle *candidate = teleop_gc_active_handles;
         candidate != NULL;
         candidate = candidate->next) {
        if (candidate == handle && candidate->generation == generation) {
            candidate->active_callbacks++;
            found = 1;
            break;
        }
    }
    pthread_mutex_unlock(&teleop_gc_registry_mutex);
    return found;
}

static void teleop_gc_release_callback(teleop_gc_handle *handle) {
    pthread_mutex_lock(&teleop_gc_registry_mutex);
    handle->active_callbacks--;
    pthread_cond_broadcast(&teleop_gc_registry_condition);
    pthread_mutex_unlock(&teleop_gc_registry_mutex);
}

static teleop_gc_state teleop_gc_capture(GCExtendedGamepad *gamepad) {
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
    // GameController's timestamp is the input profile's monotonic event time,
    // rather than the wall clock time at which this callback happened to run.
    state.timestamp = gamepad.lastEventTimestamp;
    return state;
}

static void teleop_gc_enqueue(teleop_gc_handle *handle, GCExtendedGamepad *gamepad) {
    @autoreleasepool {
        // Capture Objective-C properties before taking the queue lock. The
        // consumer only contends with the bounded ring-buffer mutation.
        teleop_gc_state state = teleop_gc_capture(gamepad);
        pthread_mutex_lock(&handle->mutex);
        if (!handle->closed) {
            state.sequence = ++handle->sequence;
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
}

int teleop_gc_count(void) {
    @autoreleasepool {
        return (int)[teleop_gc_controllers() count];
    }
}

int teleop_gc_info(
    int index,
    uint64_t *identifier,
    char *name,
    size_t name_size,
    char *product_category,
    size_t product_category_size,
    uint32_t *features
) {
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
        if (identifier != NULL) {
            *identifier = teleop_gc_identifier(controller);
        }
        NSString *vendor = controller.vendorName ?: @"Game controller";
        teleop_gc_copy_string(vendor, name, name_size);
        teleop_gc_copy_string(
            controller.productCategory,
            product_category,
            product_category_size
        );
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

int teleop_gc_info_by_id(
    uint64_t identifier,
    char *name,
    size_t name_size,
    char *product_category,
    size_t product_category_size,
    uint32_t *features
) {
    @autoreleasepool {
        GCController *controller = teleop_gc_controller_by_id(identifier);
        if (controller == nil) {
            return 0;
        }
        GCExtendedGamepad *gamepad = controller.extendedGamepad;
        if (gamepad == nil) {
            return 0;
        }
        NSString *vendor = controller.vendorName ?: @"Game controller";
        teleop_gc_copy_string(vendor, name, name_size);
        teleop_gc_copy_string(
            controller.productCategory,
            product_category,
            product_category_size
        );
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

void *teleop_gc_open(uint64_t identifier) {
    @autoreleasepool {
        GCController *controller = teleop_gc_controller_by_id(identifier);
        if (controller == nil) return NULL;
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
        handle->handler_queue = dispatch_queue_create(
            "ai.openships.teleop.gamecontroller-input",
            DISPATCH_QUEUE_SERIAL
        );
        if (handle->handler_queue == nil) {
            pthread_cond_destroy(&handle->condition);
            pthread_mutex_destroy(&handle->mutex);
            [handle->controller release];
            free(handle);
            return NULL;
        }

        // GameController exposes one callback per controller. Reject a second
        // open instead of silently replacing and permanently silencing the
        // first handle.
        pthread_mutex_lock(&teleop_gc_registry_mutex);
        for (teleop_gc_handle *candidate = teleop_gc_active_handles;
             candidate != NULL;
             candidate = candidate->next) {
            if (candidate->controller == controller) {
                pthread_mutex_unlock(&teleop_gc_registry_mutex);
                dispatch_release(handle->handler_queue);
                pthread_cond_destroy(&handle->condition);
                pthread_mutex_destroy(&handle->mutex);
                [handle->controller release];
                free(handle);
                return NULL;
            }
        }
        handle->generation = ++teleop_gc_next_generation;
        handle->next = teleop_gc_active_handles;
        teleop_gc_active_handles = handle;
        pthread_mutex_unlock(&teleop_gc_registry_mutex);

        dispatch_queue_set_specific(
            handle->handler_queue,
            &teleop_gc_handler_queue_key,
            handle,
            NULL
        );
        handle->previous_handler_queue = controller.handlerQueue;
        if (handle->previous_handler_queue != nil) {
            dispatch_retain(handle->previous_handler_queue);
        }
        controller.handlerQueue = handle->handler_queue;

        uint64_t generation = handle->generation;
        gamepad.valueChangedHandler = ^(GCExtendedGamepad *changed, GCControllerElement *element) {
            (void)element;
            if (!teleop_gc_acquire_callback(handle, generation)) return;
            teleop_gc_enqueue(handle, changed);
            teleop_gc_release_callback(handle);
        };
        handle->disconnect_observer = [[[NSNotificationCenter defaultCenter]
            addObserverForName:GCControllerDidDisconnectNotification
            object:controller
            queue:nil
            usingBlock:^(NSNotification *note) {
                (void)note;
                if (!teleop_gc_acquire_callback(handle, generation)) return;
                pthread_mutex_lock(&handle->mutex);
                handle->disconnected = 1;
                pthread_cond_broadcast(&handle->condition);
                pthread_mutex_unlock(&handle->mutex);
                teleop_gc_release_callback(handle);
            }] retain];
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
        pthread_mutex_lock(&handle->mutex);
        handle->closed = 1;
        pthread_cond_broadcast(&handle->condition);
        pthread_mutex_unlock(&handle->mutex);

        handle->controller.extendedGamepad.valueChangedHandler = nil;
        if (handle->disconnect_observer != nil) {
            [[NSNotificationCenter defaultCenter] removeObserver:handle->disconnect_observer];
            [handle->disconnect_observer release];
            handle->disconnect_observer = nil;
        }
        if (dispatch_get_specific(&teleop_gc_handler_queue_key) != handle) {
            dispatch_sync(handle->handler_queue, ^{});
        }

        pthread_mutex_lock(&teleop_gc_registry_mutex);
        teleop_gc_handle **candidate = &teleop_gc_active_handles;
        while (*candidate != NULL && *candidate != handle) {
            candidate = &(*candidate)->next;
        }
        if (*candidate == handle) {
            *candidate = handle->next;
        }
        while (handle->active_callbacks != 0) {
            pthread_cond_wait(
                &teleop_gc_registry_condition,
                &teleop_gc_registry_mutex
            );
        }
        pthread_mutex_unlock(&teleop_gc_registry_mutex);

        handle->controller.handlerQueue = handle->previous_handler_queue;
        if (handle->previous_handler_queue != nil) {
            dispatch_release(handle->previous_handler_queue);
            handle->previous_handler_queue = nil;
        }
        dispatch_release(handle->handler_queue);
        handle->handler_queue = nil;
        [handle->controller release];
        handle->controller = nil;
        pthread_cond_destroy(&handle->condition);
        pthread_mutex_destroy(&handle->mutex);
        free(handle);
    }
}
