import { expect, type Locator } from "@playwright/test";

// A reservation's status is asserted where it is shown, in a Reservation column or card. This was
// getByText(status), which also matches the card's description ("... released when the order is
// cancelled"), so it passed on a page with no reservation at all.
export function expectReservationStatus(scope: Locator, status: string, options?: { timeout?: number }) {
  return expect(scope.getByText(status)).toBeVisible(options);
}
