# Runbook: a limit while reserved, on an Alpitronic HYC50

**The question:** can a charging limit be set while a connector is *reserved*, and does it
hold once the reservation is redeemed?

**Before you start:** `cpms run` is running, the station is connected, and you have the
RFID card you will charge with (`<card>` below). With more than one EVSE configured, add
`--evse` to every command.

!!! danger "Step 12 (clean up) is mandatory"
    The HYC50 keeps charging profiles across reboots. A limit left behind caps every later
    session on the connector, and nothing in `cpms status` points to it once cpms has
    forgotten it. Clean up also after a failed or aborted run.

## What the HYC50 reports

| Key | Value |
|---|---|
| `ChargingScheduleAllowedChargingRateUnit` | `Power,Current` |
| `ChargeProfileMaxStackLevel` | 9 |
| `ChargingScheduleMaxPeriods` | 10 |
| `MaxChargingProfilesInstalled` | 100 |

Its `SupportedFeatureProfiles` is cut short at five entries, so `cpms probe` may show
SmartCharging as `?` although the station supports it. Go by the four keys above.

## Steps

### 0. Turn on the readings

The limit is checked on the meter: the `METER` column of `cpms status`
(see [Read power and current](metering.md)).

First run `cpms probe --raw` and **note the original values** of `MeterValuesSampledData`,
`MeterValuesAlignedData` and `ClockAlignedDataInterval`. Step 12 puts them back. Then:

```sh
cpms config set MeterValuesSampledData Energy.Active.Import.Register,Power.Active.Import,Power.Offered,Current.Import,Current.Offered
cpms config set MeterValuesAlignedData Power.Offered,Current.Offered
cpms config set ClockAlignedDataInterval 60
```

Sampled data shows power drawn and offered during a session. The aligned pair is the only
reading on a `Reserved` connector. The HYC50 may send aligned values for connector 0 too:
that is the station's own meter, on the first line of `cpms status`. Read the limit from
the EVSE row.

### 1. Check the station

`cpms probe`: the four keys above, and SmartCharging `✓` or `?`. `cpms limit show` should
answer (exit 0).

Exit 11 only means the station does not answer `GetCompositeSchedule`. Carry on, and read
the limit from the meter instead of `limit show`.

### 2. Reserve

```sh
cpms reserve --tag <card> --expires 30m
```

`cpms status` shows `RESERVED`.

### 3. Set a limit while reserved

```sh
cpms limit set --kw 20
```

| Exit | Answer | Finding |
|---|---|---|
| 0 | `Accepted` | The station took a TxDefault on a reserved connector. `cpms status` still shows `RESERVED` and lists `#1 tx-default stack 0, 20.0 kW` as sent. |
| 4 | `Rejected` | It refuses profiles while reserved, or in W: try `--amps 32`. |
| 11 | | No Smart Charging. |

### 4. Ask what it applies

`cpms limit show`: a period at `20.0 kW`. No period, or a higher one, means it accepted the
profile but does not (yet) apply it to this connector.

Cross-check on the meter: within one to two aligned intervals `cpms status` shows
`offered 20.0 kW` while still `RESERVED`. No offered value at all means the station sends no
aligned data for this connector. That is not a finding about the limit.

### 5. Negative test

```sh
cpms limit set --purpose tx --kw 15 --force
```

Expected: exit 4, `Rejected` (no session to bind it to).

`Accepted` is worth noting: the station then holds a TxProfile without a session, which
would cap the session in step 7 at 15 kW. **Clear it before step 6:**
`cpms limit clear --purpose tx`, then `cpms limit show` shows 20.0 kW again.

### 6. A station cap leaves the reservation alone

```sh
cpms limit set --station --purpose station-max --kw 30
```

`cpms status`: still `RESERVED`, reservation unchanged.

### 7. Redeem

Plug in the car and present the card (or `cpms start --tag <card>`). `cpms status` shows
`CHARGING` with the session, and the reservation gone.

Wait for a sampled reading. Power drawn stays at or below 20 kW and offered power reads
20.0 kW.

!!! info "This is the finding"
    If the station draws or offers more than 20 kW, it accepted the TxDefault but did not
    apply it to the session that redeemed the reservation.

### 8. Tighten during the session

```sh
cpms limit set --purpose tx --kw 10
```

Exit 0. Within a `MeterValueSampleInterval` the meter shows `offered 10.0 kW`, the power
drawn falls to 10 kW or below, and `cpms limit show` shows 10.0 kW.

### 9. Stop and clear

`cpms stop`, then `cpms status`: the `tx` record is gone with the session, `tx-default` and
`station-max` remain. `cpms limit clear --all` removes everything, and `cpms limit show`
shows no limit.

### 10. Expiry with a TxDefault set

```sh
cpms reserve --expires 2m
cpms limit set --kw 20
```

Let the reservation expire (or `cpms cancel`), then `cpms limit show`: the TxDefault should
survive the reservation ending.

### 11. Units

Repeat step 3 in the other unit (`--amps 32`). The HYC50 allows both, so both should be
`Accepted`.

### 12. Clean up

1. `cpms limit clear --all`. Exit 0, or exit 4 with `Unknown` when nothing was left.
2. `cpms limit show`: no limit. **Do not stop until it says so.**
3. `cpms cancel` if a reservation is still held.
4. Put back the meter settings you noted in step 0:

    ```sh
    cpms config set ClockAlignedDataInterval 0          # or its original
    cpms config set MeterValuesAlignedData <original>
    cpms config set MeterValuesSampledData <original>
    ```

5. Check with `cpms probe --raw`.

## Rehearse it on the simulator

```sh
cpms simulate charger --meter-interval 5s            # caps its 50 kW at the active limit
cpms simulate charger --scenario reject-profile      # answers every profile Rejected
```

Results from the real station go in
[issue #11](https://github.com/wolffseb/headless-csms/issues/11).
