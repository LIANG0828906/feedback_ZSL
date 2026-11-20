#include "battle/camera.h"

#include <glm/geometric.hpp>

#include <cmath>
#include <limits>

namespace battle {
namespace {

constexpr float kBasisEpsilon = 1.0e-7F;

Error ray_error(std::string message, std::string point) {
    Error error;
    error.code = StatusCode::RayInvalid;
    error.message = std::move(message);
    error.injection_point = std::move(point);
    return error;
}

}  // namespace

Outcome<CameraBasis> validate_camera(const Camera& camera) {
    if (!is_finite(camera.position)) {
        return Outcome<CameraBasis>::failure(ray_error(
            "camera position must be finite", "camera.position"));
    }
    if (!is_finite(camera.forward) || glm::dot(camera.forward, camera.forward) <=
                                          kBasisEpsilon * kBasisEpsilon) {
        return Outcome<CameraBasis>::failure(ray_error(
            "camera forward vector is zero or non-finite", "camera.forward"));
    }
    if (!is_finite(camera.up) ||
        glm::dot(camera.up, camera.up) <= kBasisEpsilon * kBasisEpsilon) {
        return Outcome<CameraBasis>::failure(ray_error(
            "camera up vector is zero or non-finite", "camera.up"));
    }
    if (!is_finite(camera.fov_y_radians) || camera.fov_y_radians <= kBasisEpsilon ||
        camera.fov_y_radians >= 3.14159265358979323846F - kBasisEpsilon) {
        return Outcome<CameraBasis>::failure(ray_error(
            "camera vertical FOV is degenerate", "camera.fov_y"));
    }
    if (!is_finite(camera.aspect) || camera.aspect <= kBasisEpsilon) {
        return Outcome<CameraBasis>::failure(ray_error(
            "camera aspect ratio is degenerate", "camera.aspect"));
    }
    if (!is_finite(camera.viewport_width) || !is_finite(camera.viewport_height) ||
        camera.viewport_width <= 0.0F || camera.viewport_height <= 0.0F) {
        return Outcome<CameraBasis>::failure(ray_error(
            "camera viewport is degenerate", "camera.viewport"));
    }
    if (!is_finite(camera.near_plane) || !is_finite(camera.far_plane) ||
        camera.near_plane <= 0.0F || camera.far_plane <= camera.near_plane) {
        return Outcome<CameraBasis>::failure(ray_error(
            "camera near/far planes are degenerate", "camera.clip"));
    }

    const glm::vec3 forward = glm::normalize(camera.forward);
    const glm::vec3 right = glm::cross(forward, glm::normalize(camera.up));
    const float right_length = glm::length(right);
    if (!is_finite(right) || right_length <= kBasisEpsilon) {
        return Outcome<CameraBasis>::failure(ray_error(
            "camera basis is degenerate (forward parallel to up)",
            "camera.basis"));
    }
    const glm::vec3 normalized_right = right / right_length;
    const glm::vec3 up = glm::normalize(glm::cross(normalized_right, forward));

    CameraBasis basis;
    basis.position = camera.position;
    basis.forward = forward;
    basis.up = up;
    basis.right = normalized_right;
    basis.tan_half_fov = std::tan(camera.fov_y_radians * 0.5F);
    basis.aspect = camera.aspect;
    basis.viewport_width = camera.viewport_width;
    basis.viewport_height = camera.viewport_height;
    basis.near_plane = camera.near_plane;
    basis.far_plane = camera.far_plane;
    if (!is_finite(basis.tan_half_fov) || basis.tan_half_fov <= 0.0F) {
        return Outcome<CameraBasis>::failure(ray_error(
            "camera projection state is non-finite", "camera.projection"));
    }
    return Outcome<CameraBasis>::success(basis);
}

Outcome<Ray> screen_to_ray(const Camera& camera, ScreenPoint screen) {
    Outcome<CameraBasis> validated = validate_camera(camera);
    if (!validated) {
        return Outcome<Ray>::failure(validated.error());
    }
    if (!is_finite(screen.x) || !is_finite(screen.y)) {
        return Outcome<Ray>::failure(ray_error(
            "screen coordinate must be finite", "ray.screen.nonfinite"));
    }
    if (screen.x < 0.0F || screen.x > camera.viewport_width ||
        screen.y < 0.0F || screen.y > camera.viewport_height) {
        return Outcome<Ray>::failure(ray_error(
            "screen coordinate is outside the viewport", "ray.screen.bounds"));
    }

    const CameraBasis& basis = validated.value();
    const float ndc_x = 2.0F * screen.x / basis.viewport_width - 1.0F;
    const float ndc_y = 1.0F - 2.0F * screen.y / basis.viewport_height;
    glm::vec3 direction = basis.forward +
                          basis.right * (ndc_x * basis.aspect * basis.tan_half_fov) +
                          basis.up * (ndc_y * basis.tan_half_fov);
    const float length = glm::length(direction);
    if (!is_finite(direction) || !is_finite(length) || length <= kBasisEpsilon) {
        return Outcome<Ray>::failure(ray_error(
            "camera produced a zero-length or non-finite ray",
            "ray.direction"));
    }
    Ray ray;
    ray.origin = basis.position;
    ray.direction = direction / length;
    return Outcome<Ray>::success(ray);
}

Outcome<Ray> validate_ray(Ray ray, const std::string& injection_point) {
    if (!is_finite(ray.origin)) {
        return Outcome<Ray>::failure(ray_error(
            "ray origin must be finite", injection_point + ".origin"));
    }
    if (!is_finite(ray.direction)) {
        return Outcome<Ray>::failure(ray_error(
            "ray direction must be finite", injection_point + ".direction"));
    }
    const float squared_length = glm::dot(ray.direction, ray.direction);
    if (!is_finite(squared_length) || squared_length <= kBasisEpsilon * kBasisEpsilon) {
        return Outcome<Ray>::failure(ray_error(
            "ray direction has zero length", injection_point + ".zero_length"));
    }
    ray.direction /= std::sqrt(squared_length);
    if (!is_finite(ray.direction)) {
        return Outcome<Ray>::failure(ray_error(
            "normalized ray direction is non-finite",
            injection_point + ".normalized"));
    }
    return Outcome<Ray>::success(ray);
}

Outcome<glm::vec2> project_to_screen(const Camera& camera,
                                     const glm::vec3& point) {
    Outcome<CameraBasis> validated = validate_camera(camera);
    if (!validated) {
        return Outcome<glm::vec2>::failure(validated.error());
    }
    if (!is_finite(point)) {
        return Outcome<glm::vec2>::failure(ray_error(
            "point to project must be finite", "camera.project.point"));
    }
    const CameraBasis& basis = validated.value();
    const glm::vec3 relative = point - basis.position;
    const float depth = glm::dot(relative, basis.forward);
    if (!is_finite(depth) || depth <= basis.near_plane) {
        return Outcome<glm::vec2>::failure(ray_error(
            "point is behind the camera near plane", "camera.project.depth"));
    }
    const float ndc_x = glm::dot(relative, basis.right) /
                        (depth * basis.tan_half_fov * basis.aspect);
    const float ndc_y = glm::dot(relative, basis.up) /
                        (depth * basis.tan_half_fov);
    glm::vec2 screen;
    screen.x = (ndc_x + 1.0F) * 0.5F * basis.viewport_width;
    screen.y = (1.0F - ndc_y) * 0.5F * basis.viewport_height;
    if (!is_finite(screen.x) || !is_finite(screen.y)) {
        return Outcome<glm::vec2>::failure(ray_error(
            "projected screen coordinate is non-finite",
            "camera.project.screen"));
    }
    return Outcome<glm::vec2>::success(screen);
}

Camera make_look_at_camera(const glm::vec3& position, const glm::vec3& target,
                           float fov_y_radians, float aspect,
                           float viewport_width, float viewport_height) {
    Camera camera;
    camera.position = position;
    camera.forward = target - position;
    camera.up = glm::vec3(0.0F, 1.0F, 0.0F);
    camera.fov_y_radians = fov_y_radians;
    camera.aspect = aspect;
    camera.viewport_width = viewport_width;
    camera.viewport_height = viewport_height;
    return camera;
}

}  // namespace battle
