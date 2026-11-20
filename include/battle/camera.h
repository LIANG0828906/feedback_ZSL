#pragma once

#include "battle/bvh.h"

#include <glm/vec2.hpp>

#include <cstdint>
#include <string>

namespace battle {

struct ScreenPoint {
    float x = 0.0F;
    float y = 0.0F;
};

struct Camera {
    glm::vec3 position{0.0F, 10.0F, 10.0F};
    glm::vec3 forward{0.0F, -0.5F, -1.0F};
    glm::vec3 up{0.0F, 1.0F, 0.0F};
    float fov_y_radians = 0.785398163F;
    float aspect = 16.0F / 9.0F;
    float viewport_width = 1280.0F;
    float viewport_height = 720.0F;
    float near_plane = 0.01F;
    float far_plane = 1000.0F;
};

struct CameraBasis {
    glm::vec3 position;
    glm::vec3 forward;
    glm::vec3 up;
    glm::vec3 right;
    float tan_half_fov = 0.0F;
    float aspect = 0.0F;
    float viewport_width = 0.0F;
    float viewport_height = 0.0F;
    float near_plane = 0.0F;
    float far_plane = 0.0F;
};

Outcome<CameraBasis> validate_camera(const Camera& camera);
Outcome<Ray> screen_to_ray(const Camera& camera, ScreenPoint screen);
Outcome<Ray> validate_ray(Ray ray, const std::string& injection_point);
Outcome<glm::vec2> project_to_screen(const Camera& camera, const glm::vec3& point);
Camera make_look_at_camera(const glm::vec3& position, const glm::vec3& target,
                           float fov_y_radians, float aspect,
                           float viewport_width, float viewport_height);

}  // namespace battle

