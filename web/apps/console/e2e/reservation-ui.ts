import { expect, type Locator } from "@playwright/test";

// A reservation's status is asserted on its badge (data-testid="reservation-status") and as the
// whole text, never by searching the scope for the word: getByText("RELEASED") also matches the
// order card's description ("... released when the order is cancelled"), which passed on a page
// with no reservation at all and failed (strict mode) once the badge was there too. `scope` must
// hold one reservation badge; a list row does, a page of rows does not.
export function expectReservationStatus(scope: Locator, status: string, options?: { timeout?: number }) {
  return expect(scope.getByTestId("reservation-status")).toHaveText(status, options);
}
