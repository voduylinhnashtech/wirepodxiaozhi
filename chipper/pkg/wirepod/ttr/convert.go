package wirepod_ttr

import (
	"encoding/binary"
	"math"
)

func bytesToInt16s(data []byte) []int16 {
	int16s := make([]int16, len(data)/2)
	for i := range int16s {
		int16s[i] = int16(binary.LittleEndian.Uint16(data[i*2 : i*2+2]))
	}
	return int16s
}

func int16sToBytes(data []int16) []byte {
	bytes := make([]byte, len(data)*2)
	for i, val := range data {
		binary.LittleEndian.PutUint16(bytes[i*2:], uint16(val))
	}
	return bytes
}

// resample24kTo8kSimple - Downsample from 24kHz to 8kHz for Vector ExternalAudioStreamPlayback.
//
// Note: The original implementation used linear interpolation. For a fixed 24k→8k (ratio 3:1),
// a simple box filter (average-of-3) acts as a lightweight low-pass and reduces aliasing/harshness
// ("rè") compared to naive decimation.
func resample24kTo8kSimple(input []byte) [][]byte {
	int16s := bytesToInt16s(input)
	// Fixed ratio 24k→8k: every 3 input samples -> 1 output sample (with simple low-pass).
	newLength := len(int16s) / 3
	if newLength <= 0 {
		return nil
	}
	output := make([]int16, newLength)
	for i := 0; i < newLength; i++ {
		base := i * 3
		// Box filter: average 3 samples using int32 to avoid overflow.
		sum := int32(int16s[base]) + int32(int16s[base+1]) + int32(int16s[base+2])
		output[i] = int16(sum / 3)
	}

	// Apply dynamic range compression + gain to increase volume without distortion.
	// This makes audio louder while preserving clarity better than simple gain.
	outBytes := compressAndBoost(int16sToBytes(output), 1.8)
	var audioChunks [][]byte
	// Chunk into 1024 bytes (like Play Audio)
	// Don't pad with zeros - send exact size (padding zeros can cause audio issues)
	for len(outBytes) >= 1024 {
		audioChunks = append(audioChunks, outBytes[:1024])
		outBytes = outBytes[1024:]
	}
	// If there's remaining data < 1024 bytes, include it (will be sent via flush timer)
	if len(outBytes) > 0 {
		audioChunks = append(audioChunks, outBytes)
	}
	return audioChunks
}

// downsample24kTo16kSimple - Simple downsample without filter/volume processing (like Play Audio)
// This preserves original audio quality by only doing linear downsample
func downsample24kTo16kSimple(input []byte) [][]byte {
	outBytes := downsample24kTo16kLinear(input)
	var audioChunks [][]byte
	// No filter, no volume increase - just downsample and chunk (like Play Audio)
	for len(outBytes) > 0 {
		if len(outBytes) < 1024 {
			chunk := make([]byte, 1024)
			copy(chunk, outBytes)
			audioChunks = append(audioChunks, chunk)
			break
		}
		audioChunks = append(audioChunks, outBytes[:1024])
		outBytes = outBytes[1024:]
	}
	return audioChunks
}

func downsample24kTo16k(input []byte) [][]byte {
	outBytes := downsample24kTo16kLinear(input)
	var audioChunks [][]byte
	// Apply low-pass filter to prevent aliasing (necessary for quality)
	filteredBytes := lowPassFilter(outBytes, 4000, 16000)
	// Use lower volume factor (1.0 = no change) to preserve original quality
	// Volume factor 5 was too high and caused distortion
	iVolBytes := increaseVolume(filteredBytes, 1.0)
	for len(iVolBytes) > 0 {
		if len(iVolBytes) < 1024 {
			chunk := make([]byte, 1024)
			copy(chunk, iVolBytes)
			audioChunks = append(audioChunks, chunk)
			break
		}
		audioChunks = append(audioChunks, iVolBytes[:1024])
		iVolBytes = iVolBytes[1024:]
	}

	return audioChunks
}

func increaseVolume(data []byte, factor float64) []byte {
	int16s := bytesToInt16s(data)

	for i := range int16s {
		scaled := float64(int16s[i]) * factor
		if scaled > math.MaxInt16 {
			int16s[i] = math.MaxInt16
		} else if scaled < math.MinInt16 {
			int16s[i] = math.MinInt16
		} else {
			int16s[i] = int16(scaled)
		}
	}

	return int16sToBytes(int16s)
}

// compressAndBoost applies dynamic range compression + gain boost to increase volume
// while preserving clarity. This is better than simple gain multiplication.
// - Compression ratio: 2:1 (quieter parts boosted more than loud parts)
// - Threshold: -12dB (compress signals above this)
// - Makeup gain: applied after compression
func compressAndBoost(data []byte, makeupGain float64) []byte {
	int16s := bytesToInt16s(data)

	// Find peak level for normalization
	maxAbs := float64(0)
	for _, sample := range int16s {
		abs := math.Abs(float64(sample))
		if abs > maxAbs {
			maxAbs = abs
		}
	}

	// If audio is too quiet, normalize first
	normalizeFactor := 1.0
	if maxAbs > 0 && maxAbs < 10000 { // If peak is below ~30% of max
		normalizeFactor = 20000.0 / maxAbs // Normalize to ~60% of max
		if normalizeFactor > 3.0 {
			normalizeFactor = 3.0 // Cap normalization to avoid excessive boost
		}
	}

	// Compression parameters
	threshold := 0.3 * math.MaxInt16 // -12dB threshold
	ratio := 2.0                     // 2:1 compression ratio

	// Apply compression + normalization + makeup gain
	for i := range int16s {
		sample := float64(int16s[i])

		// Normalize first
		sample *= normalizeFactor

		// Apply compression (soft knee)
		absSample := math.Abs(sample)
		if absSample > threshold {
			// Compress above threshold
			excess := absSample - threshold
			compressedExcess := excess / ratio
			if sample > 0 {
				sample = threshold + compressedExcess
			} else {
				sample = -(threshold + compressedExcess)
			}
		}

		// Apply makeup gain
		sample *= makeupGain

		// Clamp to prevent overflow
		if sample > math.MaxInt16 {
			int16s[i] = math.MaxInt16
		} else if sample < math.MinInt16 {
			int16s[i] = math.MinInt16
		} else {
			int16s[i] = int16(sample)
		}
	}

	return int16sToBytes(int16s)
}

// this is copied
func lowPassFilter(data []byte, cutoffFreq float64, sampleRate int) []byte {
	int16s := bytesToInt16s(data)
	filtered := make([]int16, len(int16s))
	rc := 1.0 / (2 * 3.1416 * cutoffFreq)
	dt := 1.0 / float64(sampleRate)
	alpha := dt / (rc + dt)
	filtered[0] = int16s[0]
	for i := 1; i < len(int16s); i++ {
		current := alpha*float64(int16s[i]) + (1-alpha)*float64(filtered[i-1])
		filtered[i] = int16(current)
	}

	return int16sToBytes(filtered)
}

// copied too
func downsample24kTo16kLinear(input []byte) []byte {
	int16s := bytesToInt16s(input)
	outputLength := (len(int16s) * 2) / 3
	output := make([]int16, outputLength)

	j := 0
	for i := 0; i < len(int16s)-2; i += 3 {
		first := (2*int32(int16s[i]) + int32(int16s[i+1])) / 3
		second := (int32(int16s[i+1]) + 2*int32(int16s[i+2])) / 3
		output[j] = int16(first)
		output[j+1] = int16(second)
		j += 2
	}

	return int16sToBytes(output)
}
