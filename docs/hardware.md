# Hardware: the Rogue carrier and the Xavier

Ports, switches, power and recovery on a Connect Tech Rogue (AGX101) with a Jetson AGX Xavier 32 GB. Read it before touching a board.

## Top edge, left to right (user side)

| Part | What | Notes |
| --- | --- | --- |
| P4 micro USB | Serial debug console | On-board FTDI to the Xavier's debug UART, 115200 8N1. Cable it to the PC; `scripts/console`. |
| J3 USB-C | USB 3.1 host **and flashing port** | Device mode only when Force Recovery is held at power-on; then the PC sees `0955:7019 NVIDIA Corp. APX`. A clean power-up turns it back into a host port. 1.5 A. |
| D10, D9 | LEDs | D9 input power good; D10 system power good / booting. |
| S5 / S6 / S3 | Force Recovery / Reset / Power buttons | P12 brings the same three out to a 4-pin PicoBlade (pin 1 GND, 2 FR, 3 RST, 4 PWR). |
| S1 | 4-way DIP, power mode | Ships all OFF = auto power-on. S1-1 ON = wait for the power button. S1-2..4 are debug only: leave OFF. |
| J4A, J4B USB-C | USB 3.1 host only | 3 A on one port, or 1.5 A each. No display, no device mode. |
| P11 | 12 V fan, 4-pin PicoBlade | 1 GND, 2 +12 V, 3 TACH, 4 PWM. **12 V fans only.** |

Elsewhere: S7 microSD/UFS, P10 M.2 E-key (2230 Wi-Fi/BT: PCIe x1 + USB 2.0), P1 MIPI CSI
camera expansion, P3 misc I/O (UART1/2 → `/dev/ttyTHS0`/`ttyTHS1`, I2C, SPI, GPIO), P2 CAN,
P7 RTC battery (3 V). Without one, a 30 s power cut puts the PMIC's RTC back to 2000-01-01 (`console.md`, Clock).

## Bottom edge

HDMI 2 (P5B), HDMI 1 (P5A), GbE 2 (J1B, Intel I210 on PCIe), GbE 1 (J1A, the module's own
MAC), power input P6 (Mini-Fit Jr, **9–19 V**, pins 1–2 GND, 3–4 +VIN; a 12 V 4-pin ATX lead fits).

HDMI is up to 4K60, dual display. **Nothing appears on HDMI until the Linux kernel starts**:
the Xavier bootloader only drives display head 0, which CTI carriers don't wire to HDMI.

## Module side

J2A / J2B M.2 M-key 2280, NVMe only (PCIe Gen 3), activity LEDs D6A / D6B. P8 5 V fan
(dev-kit fan), **5 V fans only**, and it must be connected before an NVMe goes in J2A; it is
not fitted on the AGX111 variant. S2 CAN termination (both off).

## Temperature and fan

- **`nvfancontrol`** is the only thing driving the fan, open loop, with the `quiet` profile from
  `/etc/nvpower/nvfancontrol/nvfancontrol_p2888.conf`: PWM 77 at 50 °C, 120 at 63, 160 at 72, 255 at
  81. `nvpower` links that config at boot. `build-rootfs` fails if either service isn't enabled.
- **Trip points** in CTI's device tree: passive (the kernel slows things down), then critical:

| Sensor | Passive | Critical |
| --- | --- | --- |
| `CPU-therm` | 90 °C | 95.5 °C |
| `GPU-therm` | 92.5 °C | 98 °C |
| `AUX-therm` | 89 °C | 94.5 °C |

- **`patai-thermal-log`** keeps each unit's history in `/persist/logs/thermal/thermal.csv`: a row a
  minute with that minute's peaks of CPU, GPU, AUX, AO and board temperature, fan PWM and throttling.
  `throttle` is the total state of the cooling devices that act (CPU and GPU frequency, memory
  bandwidth, the throttle and shutdown alerts), so anything above 0 means the kernel was slowing
  something down. It leaves out the fan and `hot-surface-alert`, a 70 °C notice that changes nothing.
- It stays on `/persist` so it works with no NVMe, and survives updates. At most 8 files of 1 MiB,
  about 80 days. Rows are written every 15 minutes, or every minute at 85 °C and above, so a cool unit
  writes about 100 small appends a day. It's PWM, not rpm, because the tach is unusable here. Rows
  still in RAM at a power cut are lost.
- Nothing depends on it and the health check ignores it, so it can't hold up a boot or cause a
  rollback. It sends nothing off the unit.

## Force Recovery (for flashing)

1. Power fully off.
2. Cable J3 to the host.
3. Hold S5 (FR), apply power, release after 3 s.
4. `lsusb -d 0955:7019` shows the Xavier.
5. After flashing, power-cycle to return J3 to host mode.

A module with nothing bootable on its eMMC falls back to recovery by itself.

## Power

P6 takes 9-19 V (CTI user guide, pages 8 and 30), with reverse polarity protection. The module alone is
rated 9-20 V (NVIDIA data sheet). About 7.5 W idle (10 W mode, one display) up to 64 W loaded (30 W mode,
everything busy); 75 W theoretical maximum. CTI sells a 19 V / 120 W brick (MSG085).

**Feed P6 at 15 V or more for MAXN.** The module's input rail is regulated to about 12.1 V on the carrier
(inferred). It holds from 14 V up at full load and is gone by 13.5 V. Below that the rail sags 0.13 V +
0.40 V per amp. MAXN under combined load reset the unit on a 12 V 6 A supply and on the RD6024 at a steady
12.00 V, so a stronger 12 V supply won't help, while 18 V ran clean. Everything else runs on 12 V, the default
15 W mode at full load included.

Peak measured is 67.1 W, and 5.37 A at 12.5 V in. A 19 V 120 W supply covers that and CTI's 75 W maximum
with margin for fast spikes, which the 9 Hz logging can't see.

Module rails read up to 46.7 W at MAXN with the GPU and 4 CPU cores busy. They don't include the
carrier, NVMe or fan. The INA3221 channels labelled GPU, CPU, SOC, CV and VDDRQ sit on the module's input
rail, so `in1_input` reads that rail, not P6 (root only, about a 0.5 s average).

## Shutting down

**`poweroff` doesn't stay off** with S1 at its factory auto power-on setting: the unit reaches
`reboot: Power down` and cold-boots straight away. To cut power by hand, run `sudo systemctl halt`, wait
for `reboot: System halted` on the console, then cut power within 2 minutes: the watchdog is enabled and
restarts a halted unit after about 150 s (`Tegra WDT init timeout = 120 sec`). Shut down
on the console's System page halts it and says when it has (`console.md`, Shutting down).

## Telling resets apart

`dmesg | grep -E 'PMC reset source|NVERC'` after a boot:

| PMC source | PMIC `NVERC` | Means |
| --- | --- | --- |
| `MAINSWRST` L1 | `0x0` | Software reboot, including a panic with `panic=10` |
| `BCCPLEXWDT` | `0x0` | Watchdog: the kernel hung, or a `halt` ran out of time |
| `SYS_RESET_N` L0 | `0x40` | `MBU`, input undervoltage, as after a power cycle by hand |
| `SYS_RESET_N` L0 | `0x4` | `HDRST`: the module powered down and came back, as in a MAXN load reset on too low a supply |
| `SYS_RESET_N` L0 | `0x5b` | Several bits at once: input power cut |

A `MAINSWRST` boot can read `0x40` too. Bit names are from NVIDIA's T186 CBoot table (B0 `SHDN`, `WDOG`,
`HDRST`, `TOVLD`, `MBSLD`, `MBO`, `MBU`, B7 `RSTIN`), assumed to hold for this board's MAX20024.
