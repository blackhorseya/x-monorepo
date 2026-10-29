import { useQuery } from "@tanstack/react-query";
import { useCredentialGeneration } from "../auth";
import { adminProbeQuery, whoAmIQuery } from "../queries";

// May the current credential name any customer when it creates an order? An admin may (orders on
// behalf of a customer), and so must an anonymous caller (auth off / optional: nobody to default
// to). Everyone else is the customer: the server fills in their own subject and refuses another
// (order.permission_denied), so the form locks the field. Both queries are keyed by the credential
// generation, so a late answer for the previous credential never unlocks it for the new one.
// A courtesy, not the gate: the server decides.
export function useCanChooseCustomer(): boolean {
  const generation = useCredentialGeneration();
  const admin = useQuery(adminProbeQuery(generation)).data === true;
  const anonymous = useQuery(whoAmIQuery(generation)).data?.anonymous === true;
  return admin || anonymous;
}
