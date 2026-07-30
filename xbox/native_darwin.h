//go:build darwin && cgo

#ifndef TELEOP_GAMECONTROLLER_H
#define TELEOP_GAMECONTROLLER_H

#include <stdint.h>
#include <stddef.h>

typedef struct {
    float left_x;
    float left_y;
    float right_x;
    float right_y;
    float left_trigger;
    float right_trigger;
    uint8_t a;
    uint8_t b;
    uint8_t x;
    uint8_t y;
    uint8_t left_bumper;
    uint8_t right_bumper;
    uint8_t left_stick;
    uint8_t right_stick;
    uint8_t menu;
    uint8_t view;
    uint8_t home;
    uint8_t share;
    uint8_t dpad_up;
    uint8_t dpad_down;
    uint8_t dpad_left;
    uint8_t dpad_right;
    uint64_t sequence;
    double timestamp;
    int dropped;
} teleop_gc_state;

int teleop_gc_count(void);
int teleop_gc_info(
    int index,
    uint64_t *identifier,
    char *name,
    size_t name_size,
    char *product_category,
    size_t product_category_size,
    uint32_t *features
);
int teleop_gc_info_by_id(
    uint64_t identifier,
    char *name,
    size_t name_size,
    char *product_category,
    size_t product_category_size,
    uint32_t *features
);
void *teleop_gc_open(uint64_t identifier);
int teleop_gc_next(void *opaque, teleop_gc_state *state, int timeout_ms);
int teleop_gc_set_rumble(
    void *opaque,
    float low_frequency,
    float high_frequency,
    char *error,
    size_t error_size
);
void teleop_gc_close(void *opaque);

#endif
