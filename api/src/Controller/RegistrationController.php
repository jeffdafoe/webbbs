<?php

namespace App\Controller;

use App\Entity\User;
use App\Entity\UserProfile;
use Doctrine\ORM\EntityManagerInterface;
use Symfony\Bundle\FrameworkBundle\Controller\AbstractController;
use Symfony\Component\HttpFoundation\JsonResponse;
use Symfony\Component\HttpFoundation\Request;
use Symfony\Component\HttpFoundation\Response;
use Symfony\Component\PasswordHasher\Hasher\UserPasswordHasherInterface;
use Symfony\Component\Routing\Attribute\Route;

class RegistrationController extends AbstractController
{
    #[Route('/api/register', name: 'api_register', methods: ['POST'])]
    public function register(
        Request $request,
        UserPasswordHasherInterface $passwordHasher,
        EntityManagerInterface $entityManager,
    ): JsonResponse {
        $data = json_decode($request->getContent(), true);

        if (empty($data['username']) || empty($data['password'])) {
            return $this->json(
                ['error' => 'Username and password are required.'],
                Response::HTTP_BAD_REQUEST
            );
        }

        $username = trim($data['username']);

        if (strlen($username) < 3 || strlen($username) > 35) {
            return $this->json(
                ['error' => 'Username must be between 3 and 35 characters.'],
                Response::HTTP_BAD_REQUEST
            );
        }

        $existing = $entityManager->getRepository(User::class)->findOneBy(['username' => $username]);
        if ($existing !== null) {
            return $this->json(
                ['error' => 'Username is already taken.'],
                Response::HTTP_CONFLICT
            );
        }

        $user = new User();
        $user->setUsername($username);
        $user->setPassword($passwordHasher->hashPassword($user, $data['password']));
        $user->setRoles(['ROLE_USER']);

        if (!empty($data['email'])) {
            $user->setEmail($data['email']);
        }

        $profile = new UserProfile();
        $profile->setUser($user);
        $user->setProfile($profile);

        $entityManager->persist($user);
        $entityManager->persist($profile);
        $entityManager->flush();

        return $this->json(
            [
                'message' => 'Registration successful.',
                'username' => $user->getUsername(),
            ],
            Response::HTTP_CREATED
        );
    }
}
