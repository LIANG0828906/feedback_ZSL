package scene

import (
	"math"

	"diffrender/errs"
)

// CameraBasis returns an orthonormal camera-to-world basis. The forward vector
// points from the eye toward LookAt; right and up follow a right-handed frame.
func CameraBasis(c Camera) (forward, right, up Vec3, err error) {
	forward, ok := c.LookAt.Sub(c.Eye).Normalized()
	if !ok {
		return Vec3{}, Vec3{}, Vec3{}, errs.New(errs.CodeParam, "<scene>", "cameras."+c.ID+".look_at", "eye and look_at do not define a direction")
	}
	right = forward.Cross(c.Up)
	if right.LengthSquared() <= 1e-24 {
		return Vec3{}, Vec3{}, Vec3{}, errs.New(errs.CodeParam, "<scene>", "cameras."+c.ID+".up", "up vector is parallel to view direction")
	}
	right, _ = right.Normalized()
	up = right.Cross(forward)
	up, _ = up.Normalized()
	return forward, right, up, nil
}

// WorldToCamera returns a row-major 4x4 matrix.
func WorldToCamera(c Camera) ([16]float64, error) {
	var out [16]float64
	forward, right, up, err := CameraBasis(c)
	if err != nil {
		return out, err
	}
	back := forward.Mul(-1)
	out[0], out[1], out[2] = right.X, right.Y, right.Z
	out[3] = -right.Dot(c.Eye)
	out[4], out[5], out[6] = up.X, up.Y, up.Z
	out[7] = -up.Dot(c.Eye)
	out[8], out[9], out[10] = back.X, back.Y, back.Z
	out[11] = -back.Dot(c.Eye)
	out[15] = 1
	return out, nil
}

// CameraFromWorldToCamera converts a row-major world-to-camera matrix to the
// canonical pinhole representation. targetDistance is only used to reconstruct
// the LookAt point along the encoded view direction.
func CameraFromWorldToCamera(id string, m []float64, fovYRad float64, width, height int, targetDistance float64) (Camera, error) {
	if len(m) != 16 {
		return Camera{}, errs.New(errs.CodeVersion, "<scene>", "cameras."+id+".world_to_camera", "expected 16 matrix values, got %d", len(m))
	}
	for _, v := range m {
		if !isFinite(v) {
			return Camera{}, errs.New(errs.CodeVersion, "<scene>", "cameras."+id+".world_to_camera", "matrix contains a non-finite value")
		}
	}
	r := Vec3{m[0], m[1], m[2]}
	u := Vec3{m[4], m[5], m[6]}
	b := Vec3{m[8], m[9], m[10]}
	if math.Abs(r.Length()-1) > 1e-5 || math.Abs(u.Length()-1) > 1e-5 || math.Abs(b.Length()-1) > 1e-5 {
		return Camera{}, errs.New(errs.CodeVersion, "<scene>", "cameras."+id+".world_to_camera", "rotation basis is not unit length")
	}
	if targetDistance <= 0 || !isFinite(targetDistance) {
		targetDistance = 1
	}
	eye := Vec3{
		-(r.X*m[3] + u.X*m[7] + b.X*m[11]),
		-(r.Y*m[3] + u.Y*m[7] + b.Y*m[11]),
		-(r.Z*m[3] + u.Z*m[7] + b.Z*m[11]),
	}
	forward := b.Mul(-1)
	return Camera{
		ID:      id,
		Eye:     eye,
		LookAt:  eye.Add(forward.Mul(targetDistance)),
		Up:      u,
		FovYRad: fovYRad,
		Width:   width,
		Height:  height,
	}, nil
}

// CameraToWorld returns the row-major inverse of WorldToCamera.
func CameraToWorld(c Camera) ([16]float64, error) {
	w2c, err := WorldToCamera(c)
	if err != nil {
		return [16]float64{}, err
	}
	var out [16]float64
	for row := 0; row < 3; row++ {
		for col := 0; col < 3; col++ {
			out[row*4+col] = w2c[col*4+row]
		}
	}
	out[3] = c.Eye.X
	out[7] = c.Eye.Y
	out[11] = c.Eye.Z
	out[15] = 1
	return out, nil
}

// MatrixToQuaternion converts a row-major camera-to-world rotation to [w,x,y,z].
// The quaternion sign is canonicalized so exported bytes are stable.
func MatrixToQuaternion(m [16]float64) [4]float64 {
	trace := m[0] + m[5] + m[10]
	var q [4]float64
	switch {
	case trace > 0:
		s := math.Sqrt(trace+1) * 2
		q[0] = 0.25 * s
		q[1] = (m[9] - m[6]) / s
		q[2] = (m[2] - m[8]) / s
		q[3] = (m[4] - m[1]) / s
	case m[0] > m[5] && m[0] > m[10]:
		s := math.Sqrt(1+m[0]-m[5]-m[10]) * 2
		q[0] = (m[9] - m[6]) / s
		q[1] = 0.25 * s
		q[2] = (m[1] + m[4]) / s
		q[3] = (m[2] + m[8]) / s
	case m[5] > m[10]:
		s := math.Sqrt(1+m[5]-m[0]-m[10]) * 2
		q[0] = (m[2] - m[8]) / s
		q[1] = (m[1] + m[4]) / s
		q[2] = 0.25 * s
		q[3] = (m[6] + m[9]) / s
	default:
		s := math.Sqrt(1+m[10]-m[0]-m[5]) * 2
		q[0] = (m[4] - m[1]) / s
		q[1] = (m[2] + m[8]) / s
		q[2] = (m[6] + m[9]) / s
		q[3] = 0.25 * s
	}
	l := math.Sqrt(q[0]*q[0] + q[1]*q[1] + q[2]*q[2] + q[3]*q[3])
	for i := range q {
		q[i] /= l
	}
	if q[0] < 0 || (q[0] == 0 && (q[1] < 0 || (q[1] == 0 && (q[2] < 0 || (q[2] == 0 && q[3] < 0))))) {
		for i := range q {
			q[i] = -q[i]
		}
	}
	return q
}

// QuaternionToCameraToWorld converts [w,x,y,z] to a row-major 4x4 matrix.
func QuaternionToCameraToWorld(q []float64) ([16]float64, error) {
	var out [16]float64
	if len(q) != 4 {
		return out, errs.New(errs.CodeVersion, "<scene>", "cameras.orientationQuat", "expected [w,x,y,z], got %d values", len(q))
	}
	for _, v := range q {
		if !isFinite(v) {
			return out, errs.New(errs.CodeVersion, "<scene>", "cameras.orientationQuat", "quaternion contains a non-finite value")
		}
	}
	l := math.Sqrt(q[0]*q[0] + q[1]*q[1] + q[2]*q[2] + q[3]*q[3])
	if l <= 1e-15 {
		return out, errs.New(errs.CodeVersion, "<scene>", "cameras.orientationQuat", "quaternion has zero length")
	}
	w, x, y, z := q[0]/l, q[1]/l, q[2]/l, q[3]/l
	out[0] = 1 - 2*(y*y+z*z)
	out[1] = 2 * (x*y - z*w)
	out[2] = 2 * (x*z + y*w)
	out[4] = 2 * (x*y + z*w)
	out[5] = 1 - 2*(x*x+z*z)
	out[6] = 2 * (y*z - x*w)
	out[8] = 2 * (x*z - y*w)
	out[9] = 2 * (y*z + x*w)
	out[10] = 1 - 2*(x*x+y*y)
	out[15] = 1
	return out, nil
}

// CameraFromCameraToWorld reconstructs a camera from a row-major camera-to-world
// transform. targetDistance preserves the source LookAt magnitude.
func CameraFromCameraToWorld(id string, m [16]float64, fovYRad float64, width, height int, targetDistance float64) (Camera, error) {
	u := Vec3{m[1], m[5], m[9]}
	b := Vec3{m[2], m[6], m[10]}
	if targetDistance <= 0 || !isFinite(targetDistance) {
		targetDistance = 1
	}
	forward := b.Mul(-1)
	return Camera{
		ID:      id,
		Eye:     Vec3{m[3], m[7], m[11]},
		LookAt:  Vec3{m[3], m[7], m[11]}.Add(forward.Mul(targetDistance)),
		Up:      u,
		FovYRad: fovYRad,
		Width:   width,
		Height:  height,
	}, nil
}
