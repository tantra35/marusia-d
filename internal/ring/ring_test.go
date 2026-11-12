package ring

import (
	"context"
	"strings"
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
	// dim входит в ключ (иначе SetScale скипается дедупом) — проверяем префикс.
	if !strings.HasPrefix(key, "muted@") || animated {
		t.Fatalf("key = %q animated = %v, want muted без анимации", key, animated)
	}
	if px[0] != MUTED_RGB {
		t.Fatalf("px[0] = %v, want приглушённый красный %v", px[0], MUTED_RGB)
	}
}

// TestStaticKeyIncludesScale — смена scale на статичной сцене обязана менять
// ключ кадра: иначе render() скипает по lastDrawn и новая яркость от датчика
// света не доедет до диодов (регрессия: ключ был голым "solid"/"muted").
func TestStaticKeyIncludesScale(t *testing.T) {
	r := New("/dev/null", 12)
	r.haActive = true
	r.solid = Color{255, 255, 255}
	r.scale = 1

	key1, _, _ := r.frame(time.Now())
	r.scale = 0.2
	key2, _, _ := r.frame(time.Now())
	if key1 == key2 {
		t.Fatalf("ключ solid не изменился при смене scale (%q), дедуп скипал бы кадр", key1)
	}

	r.haActive = false
	r.muted = true
	r.scale = 1
	key3, _, _ := r.frame(time.Now())
	r.scale = 0.2
	key4, _, _ := r.frame(time.Now())
	if key3 == key4 {
		t.Fatalf("ключ muted не изменился при смене scale (%q), дедуп скипал бы кадр", key3)
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

// TestFrameThinkingSpinner — thinking: фон в пол-яркости + один диод на
// полную, бегущий по кругу (позиция меняется со временем).
func TestFrameThinkingSpinner(t *testing.T) {
	r := New("/dev/null", 12)
	r.dialog = "thinking"
	r.scale = 1

	now := time.Now()
	key, px, animated := r.frame(now)
	if key != "" || !animated {
		t.Fatalf("key = %q animated = %v, want анимацию вращения", key, animated)
	}
	full, runner := 0, -1
	for i, p := range px {
		if p == mul(THINK_RGB, 1) {
			full, runner = full+1, i
		}
	}
	if full != 1 {
		t.Fatalf("диодов 100%% яркости = %d, want ровно один", full)
	}
	for i, p := range px {
		if i != runner && p != mul(THINK_RGB, 0.5) {
			t.Fatalf("px[%d] = %v, want фон в пол-яркости %v", i, p, mul(THINK_RGB, 0.5))
		}
	}
	// через полпериода бегущий диод переместился (позиция сместилась на пол-кольца)
	_, px2, _ := r.frame(now.Add(spinPeriod / 2))
	full2 := 0
	for _, p := range px2 {
		if p == mul(THINK_RGB, 1) {
			full2++
		}
	}
	if full2 != 1 || px2[runner] == mul(THINK_RGB, 1) {
		t.Fatalf("через полпериода диод не сместился: runner=%d, full2=%d", runner, full2)
	}
}

// TestFrameHAEffects — эффекты из HA: пульс = дыхание цветом solid,
// вращение = бегущий диод с фоном в 30% (не 50%).
func TestFrameHAEffects(t *testing.T) {
	r := New("/dev/null", 12)
	r.haActive = true
	r.haEffect = HAPulse
	r.solid = Color{255, 0, 0}
	r.scale = 1

	now := time.Now()
	key, px, animated := r.frame(now)
	if key != "" || !animated {
		t.Fatalf("ha_pulse: key = %q animated = %v, want анимацию дыхания", key, animated)
	}
	if px[0] != mul([3]byte{255, 0, 0}, breatheMin(now, haPulsePeriod, haPulseMin)) {
		t.Fatalf("ha_pulse: px[0] = %v, want цвет из HA", px[0])
	}

	r.haEffect = HASpin
	_, px, animated = r.frame(now)
	if key != "" || !animated {
		t.Fatalf("ha_spin: key = %q animated = %v, want анимацию вращения", key, animated)
	}
	full, runner := 0, -1
	for i, p := range px {
		if p == mul([3]byte{255, 0, 0}, 1) {
			full, runner = full+1, i
		}
	}
	if full != 1 {
		t.Fatalf("ha_spin: диодов 100%% яркости = %d, want ровно один", full)
	}
	for i, p := range px {
		if i != runner && p != mul([3]byte{255, 0, 0}, 0.3) {
			t.Fatalf("ha_spin: px[%d] = %v, want фон в 30%% яркости", i, p)
		}
	}

	// SetHAOff через актора возвращает сцену демону
	r.haEffect = ""
	r.haActive = false
	if scene := r.resolve(); scene != "off" {
		t.Fatalf("после HA-выкл сцена = %q, want off", scene)
	}
}

// TestFrameHAFire — «огонь»: анимация, диоды мерцают независимо
// (яркость каждого в диапазоне 0.10..1 и меняется со временем).
func TestFrameHAFire(t *testing.T) {
	r := New("/dev/null", 12)
	r.haActive = true
	r.haEffect = HAFire
	r.solid = Color{255, 0, 0}
	r.scale = 1

	now := time.Now()
	key, px, animated := r.frame(now)
	if key != "" || !animated {
		t.Fatalf("ha_fire: key = %q animated = %v, want анимацию мерцания", key, animated)
	}
	// каждый диод — цвет из HA, умноженный на коэффициент 0.10..1 (допуск
	// на округление байта); фиолетовых/чужих оттенков быть не должно
	for i, p := range px {
		k := float64(p[0]) / 255
		if p[1] != 0 || p[2] != 0 || k < 0.09 || k > 1.0 {
			t.Fatalf("ha_fire: px[%d] = %v (коэфф %.3f), want %v × [0.10..1]", i, p, k, r.solid)
		}
	}
	// диоды мерцают НЕзависимо: все двенадцать на одной яркости — не огонь
	uniform := true
	for i := 1; i < leds; i++ {
		if px[i] != px[0] {
			uniform = false
			break
		}
	}
	if uniform {
		t.Fatal("ha_fire: все диоды одной яркости — мерцание не независимо")
	}
	// картинка меняется со временем
	_, px2, _ := r.frame(now.Add(200 * time.Millisecond))
	changed := false
	for i := range px {
		if px[i] != px2[i] {
			changed = true
			break
		}
	}
	if !changed {
		t.Fatal("ha_fire: кадр не меняется со временем")
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
