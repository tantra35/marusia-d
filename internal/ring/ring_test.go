package ring

import (
	"context"
	"testing"
	"time"
)

// Тесты frame() — чистой функции «состояние → кадр» (без актора и SPI).

func TestFrameFlashOverridesScene(t *testing.T) {
	r := New("/dev/null", 12)
	r.muted = true
	now := time.Now()
	r.overlayColor = Color{255, 255, 255}
	r.overlayDur = time.Second
	r.overlayTt = now.Add(time.Second)

	key, px, animated := r.frame(now)
	if key != "" || !animated {
		t.Fatalf("key = %q animated = %v, want анимацию вспышки", key, animated)
	}
	if px[0] != [3]byte{255, 255, 255} {
		t.Fatalf("px[0] = %v, want полную яркость в начале спада", px[0])
	}
	// к концу вспышки яркость упала
	_, pxEnd, _ := r.frame(now.Add(900 * time.Millisecond))
	if pxEnd[0][0] >= px[0][0] {
		t.Fatalf("хвост вспышки %v ярче начала %v", pxEnd[0], px[0])
	}
}

// TestFlashSyncSet — синхронный Flash: команда уходит актору, вызов
// возвращается после того, как актор применил состояние.
func TestFlashSyncSet(t *testing.T) {
	r := New("/dev/null", 12)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Serve(ctx)
	time.Sleep(100 * time.Millisecond) // дать актору стартовать

	r.Flash(Color{255, 255, 255}, 800*time.Millisecond) // блокируется до ответа актора

	if r.overlayColor != (Color{255, 255, 255}) || r.overlayDur != 800*time.Millisecond {
		t.Fatalf("overlay = %v %v, want белая вспышка 800 мс", r.overlayColor, r.overlayDur)
	}
	if r.overlayTt.Before(time.Now()) {
		t.Fatalf("overlayTt = %v, должен быть в будущем", r.overlayTt)
	}
}

func TestFrameMuted(t *testing.T) {
	r := New("/dev/null", 12)
	r.muted = true
	r.scale = 1

	key, px, animated := r.frame(time.Now())
	if key != "muted" || animated {
		t.Fatalf("key = %q animated = %v, want muted без анимации", key, animated)
	}
	if px[0] != MUTED_RGB {
		t.Fatalf("px[0] = %v, want приглушённый красный %v", px[0], MUTED_RGB)
	}
}

func TestFrameAnimationsBreathing(t *testing.T) {
	r := New("/dev/null", 12)
	r.dialog = "listening"
	r.scale = 1

	now := time.Now()
	key1, px1, anim1 := r.frame(now)
	_, px2, anim2 := r.frame(now.Add(400 * time.Millisecond))
	if key1 != "" || !anim1 || !anim2 {
		t.Fatalf("key = %q animated = %v/%v, want анимацию дыхания", key1, anim1, anim2)
	}
	if px2[0] == px1[0] {
		t.Fatal("яркость дыхания не меняется между фазами")
	}
}

func TestFrameOff(t *testing.T) {
	r := New("/dev/null", 12)
	r.scale = 1

	key, px, animated := r.frame(time.Now())
	if key != "off" || animated {
		t.Fatalf("key = %q animated = %v, want off без анимации", key, animated)
	}
	if px[0] != [3]byte{} {
		t.Fatalf("px[0] = %v, want чёрный", px[0])
	}
}

// TestScaleClamp — SetScale клампит 0..1 (через актора).
func TestScaleClamp(t *testing.T) {
	r := New("/dev/null", 12)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Serve(ctx)
	time.Sleep(100 * time.Millisecond)

	r.SetScale(5) // актор клампит в [0..1]
	if r.scale != 1 {
		t.Fatalf("scale = %v, want 1 (кламп сверху)", r.scale)
	}
}

// TestMutedGetter — Muted() возвращает значение (регрессия: Serve шлёт
// в reply всегда nil → type assertion паниковал; ловилось на KEY_MUTE).
func TestMutedGetter(t *testing.T) {
	r := New("/dev/null", 12)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Serve(ctx)
	time.Sleep(100 * time.Millisecond)

	if r.Muted() {
		t.Fatal("Muted() = true, want false до мьюта")
	}
	r.SetMuted(true)
	if !r.Muted() {
		t.Fatal("Muted() = false, want true после SetMuted(true)")
	}
}
