package ring

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDesireFlashOverridesScene(t *testing.T) {
	r := New("/dev/null", 12)
	r.muted = true
	r.overlayKey = "flash:FFFFFF"
	r.overlayArgs = []string{"--brightness", "100", "--duration", "800", "--breathe", "800", "all", "FFFFFF"}
	r.overlayTt = time.Now().Add(time.Second)

	key, args, respawn := r.desire()
	if !strings.HasPrefix(key, "flash:") {
		t.Fatalf("key = %q, want flash:", key)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--brightness 100") || !strings.Contains(joined, "--breathe") {
		t.Fatalf("args = %v, want breathe-вспышка с полной яркостью", args)
	}
	if respawn {
		t.Fatalf("вспышка не должна быть respawn-анимацией")
	}
}

// TestFlashSyncSet — синхронный Flash: команда уходит актору, вызов
// возвращается после того, как актор применил оверлей.
func TestFlashSyncSet(t *testing.T) {
	r := New("/dev/null", 12)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Serve(ctx)
	time.Sleep(100 * time.Millisecond) // дать актору стартовать

	r.Flash(Color{255, 255, 255}, 800*time.Millisecond) // блокируется до ответа актора

	if !strings.HasPrefix(r.overlayKey, "flash:") || len(r.overlayArgs) == 0 {
		t.Fatalf("overlay = %q %v, want flash-оверлей с аргументами", r.overlayKey, r.overlayArgs)
	}
	if r.overlayTt.Before(time.Now()) {
		t.Fatalf("overlayTt = %v, должен быть в будущем", r.overlayTt)
	}
}

func TestDesireMuted(t *testing.T) {
	r := New("/dev/null", 12)
	r.muted = true
	r.scale = 1

	key, args, respawn := r.desire()
	if key != "muted@100" {
		t.Fatalf("key = %q, want muted@100", key)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, MUTED_HEX) {
		t.Fatalf("args = %v, want %s", args, MUTED_HEX)
	}
	if respawn {
		t.Fatalf("статичная сцена не должна перезапускаться")
	}
}

func TestDesireAnimationsRespawn(t *testing.T) {
	r := New("/dev/null", 12)
	r.dialog = "listening"
	r.scale = 1

	key, _, respawn := r.desire()
	if key != "listening@100" || !respawn {
		t.Fatalf("key = %q respawn = %v, want listening@100+respawn", key, respawn)
	}
}

func TestDesireOffOnce(t *testing.T) {
	r := New("/dev/null", 12)
	r.scale = 1

	key, args, respawn := r.desire()
	if key != "off" || respawn {
		t.Fatalf("key = %q respawn = %v, want off без respawn", key, respawn)
	}
	if !strings.Contains(strings.Join(args, " "), "off") {
		t.Fatalf("args = %v, want off", args)
	}
}

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
