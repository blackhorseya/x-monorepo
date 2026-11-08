export function formatMoney(minor: bigint | undefined, currency: string | undefined): string {
  if (minor === undefined || !currency) return "";
  return `${(Number(minor) / 100).toFixed(2)} ${currency}`;
}
