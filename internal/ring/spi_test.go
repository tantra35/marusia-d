package ring

import "testing"

// TestEncodeFrameWordReverse — кадр в DMA-буфере обязан лежать с реверсом
// каждого 8-байтного слова: DMA spicc гонит слово старшим байтом вперёд
// (vendor-схема ringled). Без реверса каналы перемешиваются.
func TestEncodeFrameWordReverse(t *testing.T) {
	px := [leds][3]byte{}
	for i := range px {
		px[i] = [3]byte{0x10, 0xAA, 0x03} // неравномерный цвет — ловит перестановку
	}
	frame := encodeFrameInto(make([]byte, 0, frameLen), px)
	if len(frame) != frameLen {
		t.Fatalf("кадр %d байт, ожидалось %d", len(frame), frameLen)
	}

	// логический кадр БЕЗ реверса — сборка как на проводе (reset+GRB+reset)
	logical := make([]byte, 0, frameLen)
	logical = append(logical, make([]byte, resetLen)...)
	for _, p := range px {
		for _, ch := range []byte{p[1], p[0], p[2]} {
			e := encodeByte(ch)
			logical = append(logical, e[:]...)
		}
	}
	logical = append(logical, make([]byte, resetLen)...)

	for w := 0; w < frameLen; w += 8 {
		for j := 0; j < 8; j++ {
			if frame[w+j] != logical[w+7-j] {
				t.Fatalf("слово %d: frame[%d]=%02x, want %02x (байт по реверсу слова)",
					w, w+j, frame[w+j], logical[w+7-j])
			}
		}
	}
}
