#pragma once

#include <atomic>
#include <cstdint>
#include <memory>

namespace meshedit {

class CancellationToken {
public:
    void cancel() { cancelled_.store(true, std::memory_order_release); }
    bool cancelled() const { return cancelled_.load(std::memory_order_acquire); }

private:
    std::atomic<bool> cancelled_{false};
};

// One lease per mesh document. tryAcquire is atomic, so a second edit or
// validation request receives E_BUSY without touching the mesh.
class TaskGate {
public:
    class Lease {
    public:
        Lease() = default;
        Lease(TaskGate* gate, CancellationToken* token)
            : gate_(gate), token_(token) {}
        Lease(const Lease&) = delete;
        Lease& operator=(const Lease&) = delete;
        Lease(Lease&& other) noexcept
            : gate_(other.gate_), token_(other.token_) {
            other.gate_ = nullptr;
            other.token_ = nullptr;
        }
        Lease& operator=(Lease&& other) noexcept {
            release();
            gate_ = other.gate_;
            token_ = other.token_;
            other.gate_ = nullptr;
            other.token_ = nullptr;
            return *this;
        }
        ~Lease() { release(); }

        void release() {
            if (gate_) gate_->release();
            gate_ = nullptr;
            token_ = nullptr;
        }
        bool active() const { return gate_ != nullptr; }
        CancellationToken* token() const { return token_; }

    private:
        TaskGate* gate_ = nullptr;
        CancellationToken* token_ = nullptr;
    };

    bool tryAcquire() {
        bool expected = false;
        return busy_.compare_exchange_strong(expected, true, std::memory_order_acq_rel);
    }

    Lease acquire(CancellationToken& token) {
        bool expected = false;
        if (!busy_.compare_exchange_strong(expected, true, std::memory_order_acq_rel)) {
            return {};
        }
        return Lease(this, &token);
    }

    void release() {
        busy_.store(false, std::memory_order_release);
    }
    bool busy() const { return busy_.load(std::memory_order_acquire); }

private:
    std::atomic<bool> busy_{false};
};

} // namespace meshedit
