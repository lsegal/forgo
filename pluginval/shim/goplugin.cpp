// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// A VST3 shim over Go c-shared libraries, for validating several Go
// runtimes in one host process with pluginval.
//
// The module's factory exposes one plugin class per Go library. pluginval
// tests every class in a module one after another in a single process.
// The first instance of any class loads every library, the way a DAW loads
// all of a project's plugins before playing it, so each class is tested
// while all of the Go runtimes are resident.
//
// The libraries sit next to this module's binary inside the bundle and are
// loaded with RTLD_LOCAL (LoadLibrary on Windows), in the order of the
// libraries array. They are never unloaded: a Go c-shared library cannot
// be.

#include "public.sdk/source/main/pluginfactory.h"
#include "public.sdk/source/vst/vstsinglecomponenteffect.h"
#include "base/source/fstreamer.h"
#include "pluginterfaces/base/ibstream.h"
#include "pluginterfaces/vst/ivstparameterchanges.h"

#include <atomic>
#include <cstdio>
#include <mutex>
#include <string>

#ifdef _WIN32
#include <windows.h>
#else
#include <dlfcn.h>
#endif

using namespace Steinberg;
using namespace Steinberg::Vst;

namespace {

typedef int (*new_fn) (void);
typedef void (*free_fn) (int);
typedef int (*process_fn) (int, void*, void*, int, int, float);

// One Go c-shared library inside the bundle.
struct GoLibrary
{
	const char* file; // base name, without the platform extension
	bool ok = false;
	new_fn newInstance = nullptr;
	free_fn freeInstance = nullptr;
	process_fn process = nullptr;
};

// gain and gain-copy are byte-identical, so the gain plugin's runtime is
// loaded twice from two paths.
GoLibrary libGain {"forgo-gain"};
GoLibrary libDrive {"forgo-drive"};
GoLibrary libGainCopy {"forgo-gain-copy"};
GoLibrary* libraries[] = {&libGain, &libDrive, &libGainCopy};
std::once_flag loadOnce;

// moduleDir returns the directory holding this module's binary, with a
// trailing separator.
std::string moduleDir ()
{
	std::string path;
#ifdef _WIN32
	HMODULE self = nullptr;
	GetModuleHandleExA (GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS |
	                        GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT,
	                    reinterpret_cast<LPCSTR> (&moduleDir), &self);
	char buf[MAX_PATH];
	DWORD n = GetModuleFileNameA (self, buf, MAX_PATH);
	path.assign (buf, n);
	return path.substr (0, path.find_last_of ("\\/") + 1);
#else
	Dl_info info {};
	if (dladdr (reinterpret_cast<void*> (&moduleDir), &info) && info.dli_fname)
		path = info.dli_fname;
	return path.substr (0, path.find_last_of ('/') + 1);
#endif
}

void* lookup (void* handle, const char* name)
{
#ifdef _WIN32
	return reinterpret_cast<void*> (GetProcAddress (static_cast<HMODULE> (handle), name));
#else
	return dlsym (handle, name);
#endif
}

void openLibrary (GoLibrary& lib)
{
#ifdef _WIN32
	std::string path = moduleDir () + lib.file + ".dll";
	void* handle = LoadLibraryA (path.c_str ());
#elif defined(__APPLE__)
	std::string path = moduleDir () + lib.file + ".dylib";
	void* handle = dlopen (path.c_str (), RTLD_NOW | RTLD_LOCAL);
#else
	std::string path = moduleDir () + lib.file + ".so";
	void* handle = dlopen (path.c_str (), RTLD_NOW | RTLD_LOCAL);
#endif
	if (!handle)
	{
		fprintf (stderr, "goplugin: cannot load %s\n", path.c_str ());
		return;
	}
	lib.newInstance = reinterpret_cast<new_fn> (lookup (handle, "ForgoPluginNew"));
	lib.freeInstance = reinterpret_cast<free_fn> (lookup (handle, "ForgoPluginFree"));
	lib.process = reinterpret_cast<process_fn> (lookup (handle, "ForgoPluginProcess"));
	lib.ok = lib.newInstance && lib.freeInstance && lib.process;
	if (!lib.ok)
		fprintf (stderr, "goplugin: %s is missing an export\n", path.c_str ());
}

// load opens every library on first use and reports whether lib's
// functions resolved.
bool load (GoLibrary& lib)
{
	std::call_once (loadOnce, [] {
		for (GoLibrary* l : libraries)
			openLibrary (*l);
	});
	return lib.ok;
}

const ParamID kParamId = 0;

// GoPlugin is a stereo or mono effect with one parameter whose processing
// runs entirely in a Go library.
class GoPlugin : public SingleComponentEffect
{
public:
	GoPlugin (GoLibrary& lib, const char16* paramName) : lib (lib), paramName (paramName) {}

	tresult PLUGIN_API initialize (FUnknown* context) SMTG_OVERRIDE
	{
		tresult result = SingleComponentEffect::initialize (context);
		if (result != kResultOk)
			return result;
		if (!load (lib))
			return kResultFalse;
		handle = lib.newInstance ();
		addAudioInput (STR16 ("Stereo In"), SpeakerArr::kStereo);
		addAudioOutput (STR16 ("Stereo Out"), SpeakerArr::kStereo);
		parameters.addParameter (paramName, nullptr, 0, kDefault, ParameterInfo::kCanAutomate,
		                         kParamId);
		return kResultOk;
	}

	tresult PLUGIN_API terminate () SMTG_OVERRIDE
	{
		if (handle)
		{
			lib.freeInstance (handle);
			handle = 0;
		}
		return SingleComponentEffect::terminate ();
	}

	tresult PLUGIN_API canProcessSampleSize (int32 symbolicSampleSize) SMTG_OVERRIDE
	{
		return symbolicSampleSize == kSample32 ? kResultTrue : kResultFalse;
	}

	tresult PLUGIN_API setBusArrangements (SpeakerArrangement* inputs, int32 numIns,
	                                       SpeakerArrangement* outputs,
	                                       int32 numOuts) SMTG_OVERRIDE
	{
		if (numIns != 1 || numOuts != 1 || inputs[0] != outputs[0])
			return kResultFalse;
		int32 channels = SpeakerArr::getChannelCount (inputs[0]);
		if (channels != 1 && channels != 2)
			return kResultFalse;
		FCast<AudioBus> (audioInputs.at (0))->setArrangement (inputs[0]);
		FCast<AudioBus> (audioOutputs.at (0))->setArrangement (outputs[0]);
		return kResultTrue;
	}

	tresult PLUGIN_API process (ProcessData& data) SMTG_OVERRIDE
	{
		if (IParameterChanges* changes = data.inputParameterChanges)
		{
			for (int32 i = 0; i < changes->getParameterCount (); i++)
			{
				IParamValueQueue* queue = changes->getParameterData (i);
				int32 offset;
				ParamValue value;
				if (queue && queue->getParameterId () == kParamId &&
				    queue->getPoint (queue->getPointCount () - 1, offset, value) == kResultTrue)
					param = static_cast<float> (value);
			}
		}
		if (data.numInputs == 0 || data.numOutputs == 0 || data.numSamples == 0)
			return kResultOk;
		AudioBusBuffers& in = data.inputs[0];
		AudioBusBuffers& out = data.outputs[0];
		out.silenceFlags = 0;
		if (!lib.process (handle, in.channelBuffers32, out.channelBuffers32, in.numChannels,
		                  data.numSamples, param))
			return kResultFalse;
		return kResultOk;
	}

	tresult PLUGIN_API setState (IBStream* state) SMTG_OVERRIDE
	{
		IBStreamer streamer (state, kLittleEndian);
		float saved = 0;
		if (!streamer.readFloat (saved))
			return kResultFalse;
		param = saved;
		setParamNormalized (kParamId, saved);
		return kResultOk;
	}

	tresult PLUGIN_API getState (IBStream* state) SMTG_OVERRIDE
	{
		// Save the controller's value: the host may have changed it
		// without processing a block since, so param can be stale.
		IBStreamer streamer (state, kLittleEndian);
		streamer.writeFloat (static_cast<float> (getParamNormalized (kParamId)));
		return kResultOk;
	}

	static constexpr float kDefault = 0.5f;

private:
	GoLibrary& lib;
	const char16* paramName;
	int handle = 0;
	std::atomic<float> param {kDefault};
};

FUnknown* createGain (void*) { return (IAudioProcessor*)new GoPlugin (libGain, STR16 ("Gain")); }
FUnknown* createDrive (void*) { return (IAudioProcessor*)new GoPlugin (libDrive, STR16 ("Drive")); }
FUnknown* createGainCopy (void*)
{
	return (IAudioProcessor*)new GoPlugin (libGainCopy, STR16 ("Gain"));
}

} // namespace

BEGIN_FACTORY_DEF ("forgo", "https://github.com/lsegal/forgo", "")

DEF_CLASS2 (INLINE_UID (0x6F72676F, 0x47617061, 0x696E0001, 0x00000001), PClassInfo::kManyInstances,
            kVstAudioEffectClass, "Forgo Go Gain", 0, PlugType::kFx, "1.0.0", kVstVersionString,
            createGain)

DEF_CLASS2 (INLINE_UID (0x6F72676F, 0x47617061, 0x696E0001, 0x00000002), PClassInfo::kManyInstances,
            kVstAudioEffectClass, "Forgo Go Drive", 0, PlugType::kFx, "1.0.0", kVstVersionString,
            createDrive)

DEF_CLASS2 (INLINE_UID (0x6F72676F, 0x47617061, 0x696E0001, 0x00000003), PClassInfo::kManyInstances,
            kVstAudioEffectClass, "Forgo Go Gain (second load)", 0, PlugType::kFx, "1.0.0",
            kVstVersionString, createGainCopy)

END_FACTORY
