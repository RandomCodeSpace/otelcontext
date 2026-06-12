/**
 * Uniform random in [0, 1) from the Web Crypto CSPRNG.
 *
 * Backoff jitter has no security stakes, but Math.random() trips SAST
 * (sonar S2245) and getRandomValues costs nothing at backoff call rates,
 * so the stronger source is the cheaper conversation. Available in every
 * supported runtime: browsers and Node >= 19 expose `crypto` globally.
 */
export function cryptoRandom(): number {
  const buf = new Uint32Array(1)
  crypto.getRandomValues(buf)
  return buf[0] / 2 ** 32
}
