import { initializeAuthentication } from "./authentication.js";
import { initializeOperations } from "./operations.js";
import { initializeNavigation } from "./navigation.js";
import { initializeMailboxes } from "./mailboxes.js";
import { initializeCallerAccess } from "./caller-access.js";
import { initializeVoices } from "./voices.js";
import { initializeAdministration } from "./administration.js";
import { initializeSecurity } from "./security.js";
import { initializeBootstrap } from "./bootstrap.js";

initializeAuthentication();
initializeOperations();
initializeNavigation();
initializeMailboxes();
initializeCallerAccess();
initializeVoices();
initializeAdministration();
initializeSecurity();
initializeBootstrap();
