#pragma once

#include <atomic>
#include <cstdint>
#include <functional>
#include <map>
#include <memory>
#include <mutex>
#include <optional>
#include <string>
#include <vector>

#include "chunkpipe/error.hpp"
#include "chunkpipe/model.hpp"
#include "chunkpipe/recovery.hpp"
#include "chunkpipe/trace.hpp"
#include "chunkpipe/visibility.hpp"

namespace chunkpipe {

struct StreamingOptions {
    unsigned workerCount = 3;
    std::uint32_t loadDelayMicros = 500;
    std::vector<std::uint32_t> failChunks;
    std::uint32_t cancelAfterLoads = 0;
    std::uint32_t maxChunksPerRun = 0;
    std::uint32_t crashAfterCommits = 0;
    std::function<void(std::uint32_t completed,
                       std::uint32_t total,
                       std::uint32_t chunkId)> onLoadProgress;
};

struct RunResult {
    Code code = Code::Ok;
    BusinessError error;
    std::shared_ptr<const FrameSnapshot> snapshot;
    std::vector<BusinessError> loadFailures;
    std::vector<std::uint32_t> newlyLoaded;
    std::vector<std::uint32_t> scheduled;
    bool cancelled = false;
};

class Pipeline {
public:
    Pipeline(SceneArchive scene,
             StreamingOptions options,
             TraceLogger* logger = nullptr);

    void seedLoadedChunks(std::vector<Chunk> chunks);
    void attachRecovery(RecoveryStore* store);
    void setCancelAfterLoads(std::uint32_t count);
    void requestCancellation();

    RunResult tryRun(const Camera& camera);

    std::shared_ptr<const FrameSnapshot> latest() const;
    Result<std::shared_ptr<const FrameSnapshot>> queryPublished(
        std::uint32_t expectedVersion,
        std::uint64_t expectedHash) const;

private:
    SceneArchive scene_;
    StreamingOptions options_;
    TraceLogger* logger_ = nullptr;
    RecoveryStore* recovery_ = nullptr;

    mutable std::mutex publishedMutex_;
    std::shared_ptr<const FrameSnapshot> published_;
    std::atomic<bool> busy_{false};
    std::atomic<bool> cancelRequested_{false};
    std::map<std::uint32_t, Chunk> loaded_;
    std::vector<std::uint32_t> loadedIds_;
    std::uint32_t nextPublishVersion_ = 1;

    RunResult runImpl(const Camera& camera);
    void publish(std::shared_ptr<FrameSnapshot> snapshot);
    void rebuildLoadedIndex();
};

} // namespace chunkpipe

