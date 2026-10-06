# Copyright 2026 The forgo Authors. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

# Builds forgo-go-plugins.vst3: the VST3 shim in shim/ plus the Go c-shared
# libraries built by run.sh, which GO_LIB_DIR must point at.

cmake_minimum_required(VERSION 3.25)

set(SMTG_ENABLE_VST3_PLUGIN_EXAMPLES OFF CACHE BOOL "")
set(SMTG_ENABLE_VST3_HOSTING_EXAMPLES OFF CACHE BOOL "")
set(SMTG_ENABLE_VSTGUI_SUPPORT OFF CACHE BOOL "")
set(SMTG_RUN_VST_VALIDATOR OFF CACHE BOOL "")
set(SMTG_CREATE_MODULE_INFO OFF CACHE BOOL "")
set(SMTG_CREATE_PLUGIN_LINK OFF CACHE BOOL "")

# The VST3 SDK is MIT licensed from 3.8 on. Set VST3_SDK_DIR to use a
# local checkout instead of fetching it.
set(VST3_SDK_DIR "" CACHE PATH "VST3 SDK checkout (fetched when empty)")
if(NOT VST3_SDK_DIR)
    include(FetchContent)
    FetchContent_Populate(vst3sdk
        GIT_REPOSITORY https://github.com/steinbergmedia/vst3sdk.git
        GIT_TAG v3.8.0_build_66
        GIT_SHALLOW TRUE
        GIT_SUBMODULES base pluginterfaces public.sdk cmake
    )
    set(VST3_SDK_DIR ${vst3sdk_SOURCE_DIR})
endif()

project(forgo-go-plugins VERSION 1.0.0)

add_subdirectory(${VST3_SDK_DIR} ${PROJECT_BINARY_DIR}/vst3sdk)
smtg_enable_vst3_sdk()

smtg_add_vst3plugin(forgo-go-plugins
    shim/goplugin.cpp
    ${VST3_SDK_DIR}/public.sdk/source/vst/vstsinglecomponenteffect.cpp
)
target_link_libraries(forgo-go-plugins PRIVATE sdk)
if(NOT WIN32)
    target_link_libraries(forgo-go-plugins PRIVATE ${CMAKE_DL_LIBS})
endif()

if(NOT GO_LIB_DIR)
    message(FATAL_ERROR "set GO_LIB_DIR to the directory holding the Go libraries")
endif()
file(GLOB go_libs ${GO_LIB_DIR}/forgo-*${CMAKE_SHARED_LIBRARY_SUFFIX})
add_custom_command(TARGET forgo-go-plugins POST_BUILD
    COMMAND ${CMAKE_COMMAND} -E copy ${go_libs} $<TARGET_FILE_DIR:forgo-go-plugins>
)
