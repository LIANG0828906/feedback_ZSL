set(VF_TESTS
  test_frame_sequence
  test_dependency
  test_manifest_version
  test_incremental_snapshot
  test_worker
)

foreach(t ${VF_TESTS})
  add_executable(${t} ${t}.cpp)
  target_link_libraries(${t} PRIVATE vforensics Qt6::Test)
  add_test(NAME ${t} COMMAND ${t})
endforeach()
